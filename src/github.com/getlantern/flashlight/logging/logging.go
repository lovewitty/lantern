package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/getlantern/appdir"
	"github.com/getlantern/flashlight/geolookup"
	"github.com/getlantern/flashlight/util"
	"github.com/getlantern/go-loggly"
	"github.com/getlantern/golog"
	"github.com/getlantern/jibber_jabber"
	"github.com/getlantern/rotator"
	"github.com/getlantern/wfilter"
)

const (
	logTimestampFormat = "Jan 02 15:04:05.000"
)

var (
	log = golog.LoggerFor("flashlight.logging")

	logFile *rotator.SizeRotator

	// logglyToken is populated at build time by crosscompile.bash. During
	// development time, logglyToken will be empty and we won't log to Loggly.
	logglyToken string

	errorOut io.Writer
	debugOut io.Writer

	lastAddr string
)

func Init() error {
	logdir := appdir.Logs("Lantern")
	log.Debugf("Placing logs in %v", logdir)
	if _, err := os.Stat(logdir); err != nil {
		if os.IsNotExist(err) {
			// Create log dir
			if err := os.MkdirAll(logdir, 0755); err != nil {
				return fmt.Errorf("Unable to create logdir at %s: %s", logdir, err)
			}
		}
	}
	logFile = rotator.NewSizeRotator(filepath.Join(logdir, "lantern.log"))
	// Set log files to 1 MB
	logFile.RotationSize = 1 * 1024 * 1024
	// Keep up to 20 log files
	logFile.MaxRotation = 20

	// Loggly has its own timestamp so don't bother adding it in message,
	// moreover, golog always write each line in whole, so we need not to care about line breaks.
	errorOut = timestamped(NonStopWriter(os.Stderr, logFile))
	debugOut = timestamped(NonStopWriter(os.Stdout, logFile))
	golog.SetOutputs(errorOut, debugOut)

	return nil
}

func Configure(addr string, cloudConfigCA string, instanceId string,
	version string, buildDate string) {
	if logglyToken == "" {
		log.Debugf("No logglyToken, not sending error logs to Loggly")
		return
	}

	if version == "" {
		log.Error("No version configured, Loggly won't include version information")
		return
	}

	if buildDate == "" {
		log.Error("No build date configured, Loggly won't include build date information")
		return
	}

	if addr == lastAddr {
		log.Debug("Logging configuration unchanged")
		return
	}

	// Using a goroutine because we'll be using waitforserver and at this time
	// the proxy is not yet ready.
	go func() {
		lastAddr = addr
		enableLoggly(addr, cloudConfigCA, instanceId, version, buildDate)
	}()
}

func Close() error {
	golog.ResetOutputs()
	return logFile.Close()
}

// timestamped adds a timestamp to the beginning of log lines
func timestamped(orig io.Writer) io.Writer {
	return wfilter.LinePrepender(orig, func(w io.Writer) (int, error) {
		return fmt.Fprintf(w, "%s - ", time.Now().In(time.UTC).Format(logTimestampFormat))
	})
}

func enableLoggly(addr string, cloudConfigCA string, instanceId string,
	version string, buildDate string) {
	if addr == "" {
		log.Error("No known proxy, won't report to Loggly")
		removeLoggly()
		return
	}

	client, err := util.PersistentHTTPClient(cloudConfigCA, addr)
	if err != nil {
		log.Errorf("Could not create proxied HTTP client, not logging to Loggly: %v", err)
		removeLoggly()
		return
	}

	log.Debugf("Sending error logs to Loggly via proxy at %v", addr)

	lang, _ := jibber_jabber.DetectLanguage()
	logglyWriter := &logglyErrorWriter{
		lang:            lang,
		tz:              time.Now().Format("MST"),
		versionToLoggly: fmt.Sprintf("%v (%v)", version, buildDate),
		client:          loggly.New(logglyToken),
	}
	logglyWriter.client.Defaults["hostname"] = "hidden"
	logglyWriter.client.Defaults["instanceid"] = instanceId
	logglyWriter.client.SetHTTPClient(client)
	addLoggly(logglyWriter)
}

func addLoggly(logglyWriter io.Writer) {
	if runtime.GOOS == "android" {
		golog.SetOutputs(logglyWriter, os.Stdout)
	} else {
		golog.SetOutputs(NonStopWriter(errorOut, logglyWriter), debugOut)
	}
}

func removeLoggly() {
	golog.SetOutputs(errorOut, debugOut)
}

type logglyErrorWriter struct {
	lang            string
	tz              string
	versionToLoggly string
	client          *loggly.Client
}

func (w logglyErrorWriter) Write(b []byte) (int, error) {
	extra := map[string]string{
		"logLevel":  "ERROR",
		"osName":    runtime.GOOS,
		"osArch":    runtime.GOARCH,
		"osVersion": "",
		"language":  w.lang,
		"country":   geolookup.GetCountry(),
		"timeZone":  w.tz,
		"version":   w.versionToLoggly,
	}
	fullMessage := string(b)

	// extract last 2 (at most) chunks of fullMessage to message, without prefix,
	// so we can group logs with same reason in Loggly
	lastColonPos := -1
	colonsSeen := 0
	for p := len(fullMessage) - 2; p >= 0; p-- {
		if fullMessage[p] == ':' {
			lastChar := fullMessage[p+1]
			// to prevent colon in "http://" and "x.x.x.x:80" be treated as seperator
			if !(lastChar == '/' || lastChar >= '0' && lastChar <= '9') {
				lastColonPos = p
				colonsSeen++
				if colonsSeen == 2 {
					break
				}
			}
		}
	}
	message := strings.TrimSpace(fullMessage[lastColonPos+1:])

	// Loggly doesn't group fields with more than 100 characters
	if len(message) > 100 {
		message = message[0:100]
	}

	firstColonPos := strings.IndexRune(fullMessage, ':')
	if firstColonPos == -1 {
		firstColonPos = 0
	}
	prefix := fullMessage[0:firstColonPos]

	m := loggly.Message{
		"extra":        extra,
		"locationInfo": prefix,
		"message":      message,
		"fullMessage":  fullMessage,
	}

	err := w.client.Send(m)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

type nonStopWriter struct {
	writers []io.Writer
}

// NonStopWriter creates a writer that duplicates its writes to all the
// provided writers, even if errors encountered while writting.
func NonStopWriter(writers ...io.Writer) io.Writer {
	w := make([]io.Writer, len(writers))
	copy(w, writers)
	return &nonStopWriter{w}
}

// Write implements the method from io.Writer.
// It never fails and always return the length of bytes passed in
func (t *nonStopWriter) Write(p []byte) (int, error) {
	for _, w := range t.writers {
		w.Write(p)
	}
	return len(p), nil
}
