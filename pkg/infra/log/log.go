// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/go-kit/log/term"
	"github.com/go-stack/stack"
	"github.com/grafana/grafana/pkg/util"
	"github.com/grafana/grafana/pkg/util/errutil"
	isatty "github.com/mattn/go-isatty"
	"gopkg.in/ini.v1"
)

var llog log.Logger
var loggersToClose []DisposableHandler
var loggersToReload []ReloadableHandler
var filters map[string]level.Option
var Root MultiLoggers

func init() {
	loggersToClose = make([]DisposableHandler, 0)
	loggersToReload = make([]ReloadableHandler, 0)
	filters = map[string]level.Option{}
	llog = log.NewLogfmtLogger(os.Stderr)
}

type LogWithFilters struct {
	val      log.Logger
	filters  map[string]level.Option
	maxLevel level.Option
}

type MultiLoggers struct {
	loggers []LogWithFilters
}

func (ml MultiLoggers) Warn(msg string, args ...interface{}) {
	for _, multilogger := range ml.loggers {
		multilogger.val.Log(level.Key(), level.WarnValue(), "msg", msg, args)
	}
}

func (ml MultiLoggers) Debug(msg string, args ...interface{}) {
	for _, multilogger := range ml.loggers {
		multilogger.val.Log(level.Key(), level.DebugValue(), "msg", msg, args)
	}
}

func (ml MultiLoggers) Error(msg string, args ...interface{}) {
	for _, multilogger := range ml.loggers {
		multilogger.val.Log(level.Key(), level.ErrorValue(), "msg", msg, args)
	}
}

func (ml MultiLoggers) Info(msg string, args ...interface{}) {
	for _, multilogger := range ml.loggers {
		multilogger.val.Log(level.Key(), level.InfoValue(), "msg", msg, args)
	}
}

func New(logger string, ctx ...interface{}) MultiLoggers {
	var newloger MultiLoggers
	for _, logWithFilter := range Root.loggers {
		logWithFilter.val = log.With(logWithFilter.val, append([]interface{}{"logger", logger}, ctx...))
		v, ok := logWithFilter.filters[logger]
		if ok {
			logWithFilter.val = level.NewFilter(logWithFilter.val, v)
		} else {
			logWithFilter.val = level.NewFilter(logWithFilter.val, logWithFilter.maxLevel)
		}
		newloger.loggers = append(newloger.loggers, logWithFilter)
	}
	return newloger
}

var logLevels = map[string]level.Option{
	"trace":    level.AllowDebug(),
	"debug":    level.AllowDebug(),
	"info":     level.AllowInfo(),
	"warn":     level.AllowWarn(),
	"error":    level.AllowError(),
	"critical": level.AllowError(),
}

func getLogLevelFromConfig(key string, defaultName string, cfg *ini.File) (string, level.Option) {
	levelName := cfg.Section(key).Key("level").MustString(defaultName)
	levelName = strings.ToLower(levelName)
	level := getLogLevelFromString(levelName)
	return levelName, level
}

func getLogLevelFromString(levelName string) level.Option {
	loglevel, ok := logLevels[levelName]

	if !ok {
		level.Error(llog).Log("Unknown log level", "level", levelName)
		return level.AllowError()
	}

	return loglevel
}

// the filter is composed with logger name and level
func getFilters(filterStrArray []string) map[string]level.Option {
	filterMap := make(map[string]level.Option)

	for _, filterStr := range filterStrArray {
		parts := strings.Split(filterStr, ":")
		if len(parts) > 1 {
			filterMap[parts[0]] = getLogLevelFromString(parts[1])
		}
	}

	return filterMap
}

func Stack(skip int) string {
	call := stack.Caller(skip)
	s := stack.Trace().TrimBelow(call).TrimRuntime()
	return s.String()
}

type Formatedlogger func(w io.Writer) log.Logger

func terminalColorFn(keyvals ...interface{}) term.FgBgColor {
	for i := 0; i < len(keyvals)-1; i += 2 {
		if keyvals[i] != level.Key() {
			continue
		}
		switch keyvals[i+1] {
		case "trace":
			return term.FgBgColor{Fg: term.DarkGray}
		case "debug":
			return term.FgBgColor{Fg: term.DarkGray}
		case "info":
			return term.FgBgColor{Fg: term.Gray}
		case "warn":
			return term.FgBgColor{Fg: term.Yellow}
		case "error":
			return term.FgBgColor{Fg: term.Red}
		case "crit":
			return term.FgBgColor{Fg: term.Gray, Bg: term.DarkRed}
		default:
			return term.FgBgColor{}
		}
	}
	return term.FgBgColor{}
}

func getLogFormat(format string) Formatedlogger {
	switch format {
	case "console":
		if isatty.IsTerminal(os.Stdout.Fd()) {
			return func(w io.Writer) log.Logger {
				return term.NewLogger(w, log.NewLogfmtLogger, terminalColorFn)
			}
		}
		return func(w io.Writer) log.Logger {
			return log.NewLogfmtLogger(w)
		}
	case "text":
		return func(w io.Writer) log.Logger {
			return log.NewLogfmtLogger(w)
		}
	case "json":
		return func(w io.Writer) log.Logger {
			return log.NewJSONLogger(log.NewSyncWriter(w))
		}
	default:
		return func(w io.Writer) log.Logger {
			return log.NewLogfmtLogger(w)
		}
	}
}

// this is for file logger only
func Close() error {
	var err error
	for _, logger := range loggersToClose {
		if e := logger.Close(); e != nil && err == nil {
			err = e
		}
	}
	loggersToClose = make([]DisposableHandler, 0)

	return err
}

// Reload reloads all loggers.
func Reload() error {
	for _, logger := range loggersToReload {
		if err := logger.Reload(); err != nil {
			return err
		}
	}

	return nil
}

func ReadLoggingConfig(modes []string, logsPath string, cfg *ini.File) error {
	if err := Close(); err != nil {
		return err
	}

	defaultLevelName, _ := getLogLevelFromConfig("log", "info", cfg)
	defaultFilters := getFilters(util.SplitString(cfg.Section("log").Key("filters").String()))

	for _, mode := range modes {
		mode = strings.TrimSpace(mode)
		sec, err := cfg.GetSection("log." + mode)
		if err != nil {
			level.Error(llog).Log("Unknown log mode", "mode", mode)
			return errutil.Wrapf(err, "failed to get config section log.%s", mode)
		}

		// Log level.
		_, leveloption := getLogLevelFromConfig("log."+mode, defaultLevelName, cfg)
		modeFilters := getFilters(util.SplitString(sec.Key("filters").String()))

		format := getLogFormat(sec.Key("format").MustString(""))

		var handler LogWithFilters
		// Generate log configuration.
		switch mode {
		case "console":
			handler.val = format(os.Stdout)
		case "file":
			fileName := sec.Key("file_name").MustString(filepath.Join(logsPath, "grafana.log"))
			dpath := filepath.Dir(fileName)
			if err := os.MkdirAll(dpath, os.ModePerm); err != nil {
				level.Error(llog).Log("Failed to create directory", "dpath", dpath, "err", err)
				return errutil.Wrapf(err, "failed to create log directory %q", dpath)
			}
			fileHandler := NewFileWriter()
			fileHandler.Filename = fileName
			fileHandler.Format = format
			fileHandler.Rotate = sec.Key("log_rotate").MustBool(true)
			fileHandler.Maxlines = sec.Key("max_lines").MustInt(1000000)
			fileHandler.Maxsize = 1 << uint(sec.Key("max_size_shift").MustInt(28))
			fileHandler.Daily = sec.Key("daily_rotate").MustBool(true)
			fileHandler.Maxdays = sec.Key("max_days").MustInt64(7)
			if err := fileHandler.Init(); err != nil {
				level.Error(llog).Log("Failed to initialize file handler", "dpath", dpath, "err", err)
				return errutil.Wrapf(err, "failed to initialize file handler")
			}

			loggersToClose = append(loggersToClose, fileHandler)
			loggersToReload = append(loggersToReload, fileHandler)
			handler.val = fileHandler
		case "syslog":
			sysLogHandler := NewSyslog(sec, format)
			loggersToClose = append(loggersToClose, sysLogHandler)
			handler.val = sysLogHandler
		}
		if handler.val == nil {
			panic(fmt.Sprintf("Handler is uninitialized for mode %q", mode))
		}

		// join default filters and mode filters together
		for key, value := range defaultFilters {
			if _, exist := modeFilters[key]; !exist {
				modeFilters[key] = value
			}
		}

		// copy joined default + mode filters into filters
		for key, value := range modeFilters {
			if _, exist := filters[key]; !exist {
				filters[key] = value
			}
		}

		handler.filters = modeFilters
		handler.maxLevel = leveloption
		// handler = LogFilterHandler(leveloption, modeFilters, handler)
		Root.loggers = append(Root.loggers, handler)
	}
	return nil
}
