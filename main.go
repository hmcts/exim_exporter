package main

import (
	"fmt"
	"gopkg.in/alecthomas/kingpin.v2"
	"io"
	stdlog "log"
	"log/syslog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"

	"github.com/hpcloud/tail"
	"github.com/shirou/gopsutil/process"
)

var (
	logPath          = kingpin.Flag("exim.log-path", "Path to the Exim log files.").Default("/var/log/exim4").Envar("EXIM_LOG_PATH").String()
	mainlog          = kingpin.Flag("exim.mainlog", "Exim main log filename in log-path directory.").Default("mainlog").Envar("EXIM_MAINLOG").String()
	rejectlog        = kingpin.Flag("exim.rejectlog", "Exim reject log filename in log-path directory.").Default("rejectlog").Envar("EXIM_REJECTLOG").String()
	paniclog         = kingpin.Flag("exim.paniclog", "Exim panic log filename in log-path directory.").Default("paniclog").Envar("EXIM_PANICLOG").String()
	eximExec         = kingpin.Flag("exim.executable", "Path name of the Exim daemon executable.").Default("exim4").Envar("EXIM_EXECUTABLE").String()
	useJournal       = kingpin.Flag("exim.use-journal", "Use the systemd journal instead of log file tailing").Envar("EXIM_USE_JOURNAL").Bool()
	syslogIdentifier = kingpin.Flag("exim.syslog-identifier", "Syslog identifier used by Exim").Default("exim").Envar("EXIM_SYSLOG_IDENTIFIER").String()
	inputPath        = kingpin.Flag("exim.input-path", "Path to Exim queue directory.").Default("/var/spool/exim4/input").Envar("EXIM_QUEUE_DIR").String()
	listenAddress    = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9636").Envar("WEB_LISTEN_ADDRESS").String()
	metricsPath      = kingpin.Flag("web.telemetry-path", "URI under which to expose metrics.").Default("/metrics").Envar("WEB_TELEMETRY_PATH").String()
	tailPoll         = kingpin.Flag("tail.poll", "Poll logs for changes instead of using inotify.").Envar("TAIL_POLL").Bool()
)

const BASE62 = "0123456789aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStTuUvVwWxXyYzZ"

var (
	eximUp = prometheus.NewDesc(
		prometheus.BuildFQName("exim", "", "up"),
		"Whether or not the main exim daemon is running",
		nil, nil,
	)
	eximQueue = prometheus.NewDesc(
		prometheus.BuildFQName("exim", "", "queue"),
		"Number of messages currently in queue",
		nil, nil,
	)
	eximProcesses = prometheus.NewDesc(
		prometheus.BuildFQName("exim", "", "processes"),
		"Number of running exim process broken down by state (delivering, handling, etc)",
		[]string{"state"}, nil,
	)
	eximMessages = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "messages_total"),
			Help: "Total number of logged messages broken down by flag (delivered, deferred, etc)",
		},
		[]string{"flag"},
	)
	eximIssues = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "issues_total"),
			Help: "Total number of logged issues broken down by type (tls, smtp, connection, etc)",
		},
		[]string{"type"},
	)
	eximReject = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "reject_total"),
			Help: "Total number of logged reject messages",
		},
	)
	eximPanic = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "panic_total"),
			Help: "Total number of logged panic messages",
		},
	)
	queueRuns = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "queue_runs"),
			Help: "Total number of queue runs noted in the logs",
		},
	)
	readErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "log_read", "errors"),
			Help: "Total number of errors encountered while reading the logs",
		},
	)
)

var processFlags = map[string]string{
	"-Mc":  "delivering",
	"-bd":  "handling",
	"-bdf": "handling",
	"-qG":  "running",
}

type Process struct {
	cmdline []string
	leader  bool
}

// map globals we can override in tests
var (
	getProcesses = func() ([]*Process, error) {
		processes, err := process.Processes()
		if err != nil {
			return nil, err
		}
		result := make([]*Process, 0)
		for _, p := range processes {
			cmdline, err := p.CmdlineSlice()
			if err != nil {
				continue
			}
			pid := int(p.Pid)
			pgid, err := syscall.Getpgid(pid)
			if err != nil {
				continue
			}
			leader := pid == pgid
			result = append(result, &Process{cmdline, leader})
		}
		return result, nil
	}
)

type Exporter struct {
	mainlog   string
	rejectlog string
	paniclog  string
	eximBin   string
	inputPath string
	logger    log.Logger
}

func NewExporter(mainlog string, rejectlog string, paniclog string, eximExec string, inputPath string, logger log.Logger) *Exporter {
	return &Exporter{
		mainlog,
		rejectlog,
		paniclog,
		eximExec,
		inputPath,
		logger,
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- eximUp
	ch <- eximQueue
	ch <- eximProcesses
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	states := e.ProcessStates()
	up := float64(0)
	if _, ok := states["daemon"]; ok {
		up = 1
	}
	ch <- prometheus.MustNewConstMetric(eximUp, prometheus.GaugeValue, up)
	for label, value := range states {
		ch <- prometheus.MustNewConstMetric(eximProcesses, prometheus.GaugeValue, value, label)
	}
	queue := e.QueueSize()
	if queue >= 0 {
		ch <- prometheus.MustNewConstMetric(eximQueue, prometheus.GaugeValue, queue)
	}
}

func (e *Exporter) ProcessStates() map[string]float64 {
	level.Debug(e.logger).Log("msg", "Reading process states")
	states := make(map[string]float64)
	processes, err := getProcesses()
	if err != nil {
		level.Error(e.logger).Log("msg", err)
		return states
	}
	for _, p := range processes {
		level.Debug(e.logger).Log("msg", "process state", "isLeader", p.leader, "cmdline", fmt.Sprintf("%v", p.cmdline))
		if len(p.cmdline) < 1 || path.Base(p.cmdline[0]) != e.eximBin {
			continue
		}
		if len(p.cmdline) < 2 {
			states["other"] += 1
		} else if state, ok := processFlags[p.cmdline[1]]; ok {
			if state == "handling" && p.leader {
				states["daemon"] += 1
			} else {
				states[state] += 1
			}
		} else {
			states["other"] += 1
		}
	}
	return states
}

func (e *Exporter) CountMessages(dirname string) float64 {
	dir, err := os.Open(dirname)
	if err != nil {
		return 0
	}
	messages, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return 0
	}
	var count float64
	for _, name := range messages {
		if len(name) == 18 && strings.HasSuffix(name, "-H") {
			count += 1
		}
	}
	return count
}

func (e *Exporter) QueueSize() float64 {
	level.Debug(e.logger).Log("msg", "Reading queue size", "path", e.inputPath)
	count := e.CountMessages(e.inputPath)
	for h := 0; h < len(BASE62); h++ {
		hashPath := filepath.Join(e.inputPath, string(BASE62[h]))
		count += e.CountMessages(hashPath)
	}
	level.Debug(e.logger).Log("msg", "Queue size", "count", count)
	return count
}

func (e *Exporter) Start() {
	if *useJournal {
		go e.TailMainLog(e.JournalTail(*syslogIdentifier, syslog.LOG_INFO))
		go e.TailRejectLog(e.JournalTail(*syslogIdentifier, syslog.LOG_NOTICE))
		go e.TailPanicLog(e.JournalTail(*syslogIdentifier, syslog.LOG_ALERT))
	} else {
		go e.TailMainLog(e.FileTail(e.mainlog))
		go e.TailRejectLog(e.FileTail(e.rejectlog))
		go e.TailPanicLog(e.FileTail(e.paniclog))
	}
}

func (e *Exporter) FileTail(filename string) chan *tail.Line {
	level.Info(e.logger).Log("msg", "Opening log", "filename", filename)
	logger := log.NewStdlibAdapter(e.logger)
	t, err := tail.TailFile(filename, tail.Config{
		Location: &tail.SeekInfo{Whence: io.SeekEnd},
		ReOpen:   true,
		Follow:   true,
		Poll:     *tailPoll,
		Logger:   stdlog.New(logger, "", stdlog.LstdFlags),
	})
	if err != nil {
		level.Error(e.logger).Log("msg", "Unable to open log", "err", err)
		os.Exit(1)
	}
	return t.Lines
}

// JournalTail conditionally defined based on the "systemd" build tag.

func (e *Exporter) TailMainLog(lines chan *tail.Line) {
	level.Info(e.logger).Log("msg", "Tail mainlog")
	for line := range lines {
		if line.Err != nil {
			level.Error(e.logger).Log("msg", "Caught error while reading mainlog", "err", line.Err)
			readErrors.Inc()
			continue
		}
		level.Debug(e.logger).Log("file", "mainlog", "msg", line.Text)
		parts := strings.SplitN(line.Text, " ", 7)
		size := len(parts)
		if size < 3 {
			level.Info(e.logger).Log("msg", "Short line reading mainlog", "msg", line.Err)
			continue
		}
		// Handle logs when PID logging is enabled
		//   no pid: "2022-04-12 22:14:21 1neNq9-0008OD-06 Completed"
		//   w/ pid: "2022-04-12 22:14:21 [4646] 1neNq9-0008OD-06 Completed"
		var index int
		if parts[2][0] == '[' {
			index = 4
		} else {
			index = 3
		}
		if size < index+1 {
			level.Info(e.logger).Log("msg", "No index part reading mainlog", "msg", line.Err)
			continue
		}
		var tmp string = ""
		if size > index+1 {
			tmp = parts[index+1]
		}
		level.Debug(e.logger).Log("file", "mainlog", "parts[index-1]", parts[index-1], "parts[index]", parts[index], "parts[index+1]", tmp)
		switch parts[index] {
		case "<=":
			if (size > index+1) && (parts[index+1] == "<>") {
				eximMessages.With(prometheus.Labels{"flag": "mailnotice"}).Inc()
			} else {
				eximMessages.With(prometheus.Labels{"flag": "arrived"}).Inc()
			}
		case "(=":
			eximMessages.With(prometheus.Labels{"flag": "fakereject"}).Inc()
		case "=>":
			eximMessages.With(prometheus.Labels{"flag": "delivered"}).Inc()
		case "->":
			eximMessages.With(prometheus.Labels{"flag": "additional"}).Inc()
		case ">>":
			eximMessages.With(prometheus.Labels{"flag": "cutthrough"}).Inc()
		case "*>":
			eximMessages.With(prometheus.Labels{"flag": "suppressed"}).Inc()
		case "**":
			eximMessages.With(prometheus.Labels{"flag": "failed"}).Inc()
		case "==":
			eximMessages.With(prometheus.Labels{"flag": "deferred"}).Inc()
		case "Completed":
			eximMessages.With(prometheus.Labels{"flag": "completed"}).Inc()
		default:
			// These messages have no associated queue id,
			//   "2022-04-12 22:14:21 Start queue run: pid=229876"
			switch parts[index-1] {
			case "SMTP":
				// Eg "SMTP protocol synchronisation error ..."
				// Eg "SMTP data timeout ..."
				// Eg "SMTP call from [ip] dropped ..."
				eximIssues.With(prometheus.Labels{"type": "smtp"}).Inc()
			case "rejected":
				// Eg "rejected EHLO from [ip]: syntactically invalid argument(s): []"
				eximIssues.With(prometheus.Labels{"type": "smtp"}).Inc()
			case "auth_login":
				// Eg "auth_login authenticator failed for ..."
				eximIssues.With(prometheus.Labels{"type": "auth"}).Inc()
			case "no":
				// Eg "no host name found for IP ..."
				eximIssues.With(prometheus.Labels{"type": "hostip"}).Inc()
			case "TLS":
				// Eg "TLS error on connection ..."
				// Eg "TLS session (gnutls_handshake): Key usage ..."
				eximIssues.With(prometheus.Labels{"type": "tls"}).Inc()
			case "Connection":
				// Eg "Connection from [ip] refused: too ...
				eximIssues.With(prometheus.Labels{"type": "connect"}).Inc()
			case "unexpected":
				// Eg "unexpected disconnection ..."
				eximIssues.With(prometheus.Labels{"type": "connect"}).Inc()
			case "Start":
				// Eg "Start queue run: pid=1234", also "End..."
				queueRuns.Inc()
			}
		}
	}
}

func (e *Exporter) TailRejectLog(lines chan *tail.Line) {
	for line := range lines {
		if line.Err != nil {
			level.Error(e.logger).Log("msg", "Caught error while reading rejectlog", "err", line.Err)
			readErrors.Inc()
			continue
		}
		level.Debug(e.logger).Log("file", "rejectlog", "msg", line.Text)
		eximReject.Inc()
	}
}

func (e *Exporter) TailPanicLog(lines chan *tail.Line) {
	for line := range lines {
		if line.Err != nil {
			level.Error(e.logger).Log("msg", "Caught error while reading paniclog", "err", line.Err)
			readErrors.Inc()
			continue
		}
		level.Debug(e.logger).Log("file", "paniclog", "msg", line.Text)
		eximPanic.Inc()
	}
}

func init() {
	prometheus.MustRegister(version.NewCollector("exim_exporter"))
	prometheus.MustRegister(eximMessages)
	prometheus.MustRegister(eximIssues)
	prometheus.MustRegister(eximReject)
	prometheus.MustRegister(eximPanic)
	prometheus.MustRegister(queueRuns)
	prometheus.MustRegister(readErrors)
}

func main() {
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)

	level.Info(logger).Log("msg", "Starting exim exporter", "version", version.Info())
	level.Info(logger).Log("msg", "Build context", "context", version.BuildContext())

	if !path.IsAbs(*mainlog) {
		*mainlog = path.Join(*logPath, *mainlog)
	}
	if !path.IsAbs(*rejectlog) {
		*rejectlog = path.Join(*logPath, *rejectlog)
	}
	if !path.IsAbs(*paniclog) {
		*paniclog = path.Join(*logPath, *paniclog)
	}

	exporter := NewExporter(
		*mainlog,
		*rejectlog,
		*paniclog,
		*eximExec,
		*inputPath,
		logger,
	)
	exporter.QueueSize()
	exporter.Start()
	prometheus.MustRegister(exporter)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html>
<head><title>Exim Exporter</title></head>
<body>
  <h1>Exim Exporter</h1>
  <p>` + version.Info() + `</p>
  <p><a href='` + *metricsPath + `'>Metrics</a></p>
</body>
</html>`))
		if err != nil {
			_ = level.Error(logger).Log("msg", err)
		}
	})
	http.Handle(*metricsPath, promhttp.Handler())
	level.Info(logger).Log("msg", "Listening", "address", listenAddress)
	level.Error(logger).Log("msg", "ListenAndServe exited", "err", http.ListenAndServe(*listenAddress, nil))
}
