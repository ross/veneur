package veneur

import (
	"bytes"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/getsentry/raven-go"
	"github.com/zenazn/goji/bind"
	"github.com/zenazn/goji/graceful"

	"github.com/pkg/profile"
)

// VERSION stores the current veneur version.
// It must be a var so it can be set at link time.
var VERSION = "dirty"

var profileStartOnce = sync.Once{}

var log = logrus.New()

// A Server is the actual veneur instance that will be run.
type Server struct {
	Workers     []*Worker
	EventWorker *EventWorker

	statsd *statsd.Client
	sentry *raven.Client

	Hostname string
	Tags     []string

	DDHostname string
	DDAPIKey   string
	HTTPClient *http.Client

	HTTPAddr    string
	ForwardAddr string
	UDPAddr     *net.UDPAddr
	RcvbufBytes int

	HistogramPercentiles []float64

	plugins   []plugin
	pluginMtx sync.Mutex

	enableProfiling bool
}

// NewFromConfig creates a new veneur server from a configuration specification.
func NewFromConfig(conf Config) (ret Server, err error) {
	ret.Hostname = conf.Hostname
	ret.Tags = conf.Tags
	ret.DDHostname = conf.APIHostname
	ret.DDAPIKey = conf.Key
	ret.HistogramPercentiles = conf.Percentiles

	ret.HTTPClient = &http.Client{
		// make sure that POSTs to datadog do not overflow the flush interval
		Timeout: conf.Interval * 9 / 10,
		// we're fine with using the default transport and redirect behavior
	}

	ret.statsd, err = statsd.NewBuffered(conf.StatsAddr, 1024)
	if err != nil {
		return
	}
	ret.statsd.Namespace = "veneur."
	ret.statsd.Tags = append(ret.Tags, "veneurlocalonly")

	// nil is a valid sentry client that noops all methods, if there is no DSN
	// we can just leave it as nil
	if conf.SentryDSN != "" {
		ret.sentry, err = raven.New(conf.SentryDSN)
		if err != nil {
			return
		}
	}

	if conf.Debug {
		log.Level = logrus.DebugLevel
	}

	if conf.EnableProfiling {
		ret.enableProfiling = true
	}

	log.Hooks.Add(sentryHook{
		c:        ret.sentry,
		hostname: ret.Hostname,
		lv: []logrus.Level{
			logrus.ErrorLevel,
			logrus.FatalLevel,
			logrus.PanicLevel,
		},
	})
	log.WithField("version", VERSION).Info("Starting server")

	log.WithField("number", conf.NumWorkers).Info("Starting workers")
	ret.Workers = make([]*Worker, conf.NumWorkers)
	for i := range ret.Workers {
		ret.Workers[i] = NewWorker(i+1, ret.statsd, log)
		// do not close over loop index
		go func(w *Worker) {
			defer func() {
				ret.ConsumePanic(recover())
			}()
			w.Work()
		}(ret.Workers[i])
	}

	ret.EventWorker = NewEventWorker(ret.statsd)
	go func() {
		defer func() {
			ret.ConsumePanic(recover())
		}()
		ret.EventWorker.Work()
	}()

	ret.UDPAddr, err = net.ResolveUDPAddr("udp", conf.UDPAddr)
	if err != nil {
		return
	}
	ret.RcvbufBytes = conf.ReadBufferSizeBytes
	ret.HTTPAddr = conf.HTTPAddr
	ret.ForwardAddr = conf.ForwardAddr

	conf.Key = "REDACTED"
	conf.SentryDSN = "REDACTED"
	log.WithField("config", conf).Debug("Initialized server")

	var svc s3iface.S3API = nil
	aws_id := conf.AWSAccessKeyId
	aws_secret := conf.AWSSecretAccessKey

	conf.AWSAccessKeyId = "REDACTED"
	conf.AWSSecretAccessKey = "REDACTED"

	if len(aws_id) > 0 && len(aws_secret) > 0 {
		sess, err := session.NewSession(&aws.Config{
			Region:      aws.String(conf.AWSRegion),
			Credentials: credentials.NewStaticCredentials(aws_id, aws_secret, ""),
		})

		if err != nil {
			log.Info("error getting AWS session: %s", err)
			svc = nil
		} else {
			log.Info("Successfully created AWS session")
			svc = s3.New(sess)

			plugin := &S3Plugin{
				logger:   log,
				svc:      svc,
				s3Bucket: conf.AWSBucket,
				hostname: ret.Hostname,
			}
			ret.registerPlugin(plugin)
		}
	} else {
		log.Info("AWS credentials not found")
	}

	if svc == nil {
		log.Info("S3 archives are disabled")
	} else {
		log.Info("S3 archives are enabled")
	}

	return
}

// HandlePacket processes each packet that is sent to the server, and sends to an
// appropriate worker (EventWorker or Worker).
func (s *Server) HandlePacket(packet []byte) {
	// This is a very performance-sensitive function
	// and packets may be dropped if it gets slowed down.
	// Keep that in mind when modifying!

	if len(packet) == 0 {
		// a lot of clients send packets that accidentally have a trailing
		// newline, it's easier to just let them be
		return
	}

	if bytes.HasPrefix(packet, []byte{'_', 'e', '{'}) {
		event, err := ParseEvent(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Error("Could not parse packet")
			s.statsd.Count("packet.error_total", 1, []string{"packet_type:event"}, 1.0)
			return
		}
		s.EventWorker.EventChan <- *event
	} else if bytes.HasPrefix(packet, []byte{'_', 's', 'c'}) {
		svcheck, err := ParseServiceCheck(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Error("Could not parse packet")
			s.statsd.Count("packet.error_total", 1, []string{"packet_type:service_check"}, 1.0)
			return
		}
		s.EventWorker.ServiceCheckChan <- *svcheck
	} else {
		metric, err := ParseMetric(packet)
		if err != nil {
			log.WithFields(logrus.Fields{
				logrus.ErrorKey: err,
				"packet":        string(packet),
			}).Error("Could not parse packet")
			s.statsd.Count("packet.error_total", 1, []string{"packet_type:metric"}, 1.0)
			return
		}
		s.Workers[metric.Digest%uint32(len(s.Workers))].PacketChan <- *metric
	}
}

// ReadSocket listens for available packets to handle.
func (s *Server) ReadSocket(packetPool *sync.Pool, reuseport bool) {
	// each goroutine gets its own socket
	// if the sockets support SO_REUSEPORT, then this will cause the
	// kernel to distribute datagrams across them, for better read
	// performance
	serverConn, err := NewSocket(s.UDPAddr, s.RcvbufBytes, reuseport)
	if err != nil {
		// if any goroutine fails to create the socket, we can't really
		// recover, so we just blow up
		// this probably indicates a systemic issue, eg lack of
		// SO_REUSEPORT support
		log.WithError(err).Fatal("Error listening for UDP")
	}
	log.WithField("address", s.UDPAddr).Info("UDP server listening")

	for {
		buf := packetPool.Get().([]byte)
		n, _, err := serverConn.ReadFrom(buf)
		if err != nil {
			log.WithError(err).Error("Error reading from UDP")
			continue
		}

		// statsd allows multiple packets to be joined by newlines and sent as
		// one larger packet
		// note that spurious newlines are not allowed in this format, it has
		// to be exactly one newline between each packet, with no leading or
		// trailing newlines
		splitPacket := NewSplitBytes(buf[:n], '\n')
		for splitPacket.Next() {
			s.HandlePacket(splitPacket.Chunk())
		}

		// the Metric struct created by HandlePacket has no byte slices in it,
		// only strings
		// therefore there are no outstanding references to this byte slice, we
		// can return it to the pool
		packetPool.Put(buf)
	}
}

// HTTPServe starts the HTTP server and listens perpetually until it encounters an unrecoverable error.
func (s *Server) HTTPServe() {
	var prf interface {
		Stop()
	}

	// We want to make sure the profile is stopped
	// exactly once (and only once), even if the
	// shutdown pre-hook does not run (which it may not)
	profileStopOnce := sync.Once{}

	if s.enableProfiling {
		profileStartOnce.Do(func() {
			prf = profile.Start()
		})

		defer func() {
			profileStopOnce.Do(prf.Stop)
		}()
	}
	httpSocket := bind.Socket(s.HTTPAddr)
	graceful.Timeout(10 * time.Second)
	graceful.PreHook(func() {

		if prf != nil {
			profileStopOnce.Do(prf.Stop)
		}

		log.Info("Terminating HTTP listener")
	})

	// Ensure that the server responds to SIGUSR2 even
	// when *not* running under einhorn.
	graceful.AddSignal(syscall.SIGUSR2, syscall.SIGHUP)
	graceful.HandleSignals()
	log.WithField("address", s.HTTPAddr).Info("HTTP server listening")
	bind.Ready()

	if err := graceful.Serve(httpSocket, s.Handler()); err != nil {
		log.WithError(err).Error("HTTP server shut down due to error")
	}

	graceful.Shutdown()
}

// Shutdown signals the server to shut down after closing all
// current connections.
func (s *Server) Shutdown() {
	// TODO(aditya) shut down workers and socket readers
	log.Info("Shutting down server gracefully")
	graceful.Shutdown()
}

// IsLocal indicates whether veneur is running as a local instance
// (forwarding non-local data to a global veneur instance) or is running as a global
// instance (sending all data directly to the final destination).
func (s *Server) IsLocal() bool {
	return s.ForwardAddr != ""
}

// SplitBytes iterates over a byte buffer, returning chunks split by a given
// delimiter byte. It does not perform any allocations, and does not modify the
// buffer it is given. It is not safe for use by concurrent goroutines.
//
//     sb := NewSplitBytes(buf, '\n')
//     for sb.Next() {
//         fmt.Printf("%q\n", sb.Chunk())
//     }
//
// The sequence of chunks returned by SplitBytes is equivalent to calling
// bytes.Split, except without allocating an intermediate slice.
type SplitBytes struct {
	buf          []byte
	delim        byte
	currentChunk []byte
	lastChunk    bool
}

// NewSplitBytes initializes a SplitBytes struct with the provided buffer and delimiter.
func NewSplitBytes(buf []byte, delim byte) *SplitBytes {
	return &SplitBytes{
		buf:   buf,
		delim: delim,
	}
}

// Next advances SplitBytes to the next chunk, returning true if a new chunk
// actually exists and false otherwise.
func (sb *SplitBytes) Next() bool {
	if sb.lastChunk {
		// we do not check the length here, this ensures that we return the
		// last chunk in the sequence (even if it's empty)
		return false
	}

	next := bytes.IndexByte(sb.buf, sb.delim)
	if next == -1 {
		// no newline, consume the entire buffer
		sb.currentChunk = sb.buf
		sb.buf = nil
		sb.lastChunk = true
	} else {
		sb.currentChunk = sb.buf[:next]
		sb.buf = sb.buf[next+1:]
	}
	return true
}

// Chunk returns the current chunk.
func (sb *SplitBytes) Chunk() []byte {
	return sb.currentChunk
}

// registerPlugin registers a plugin for use
// on the veneur server. It is blocking
// and not threadsafe.
func (s *Server) registerPlugin(p plugin) {
	s.pluginMtx.Lock()
	defer s.pluginMtx.Unlock()
	s.plugins = append(s.plugins, p)
}

func (s *Server) getPlugins() []plugin {
	s.pluginMtx.Lock()
	plugins := make([]plugin, len(s.plugins))
	copy(plugins, s.plugins)
	s.pluginMtx.Unlock()
	return plugins
}
