package log

import (
	"context"
	"fmt"
	"os"

	"github.com/tchap/zapext/zapsentry"

	"github.com/blendle/zapdriver"

	"github.com/getsentry/raven-go"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

// Logger implements Context
type Logger struct {
	*zap.Logger
	Sentry *raven.Client

	dsn   string
	debug bool
	local bool
	nop   bool
}

// CtxLoggerKey defines the key under which the logger is being stored
type CtxLoggerKey string

// DefaultCtxLoggerKey defines the key under which the logger is being stored
var DefaultCtxLoggerKey = CtxLoggerKey("sm-ctx-logger")

// WithLogger returns context containing Logger
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, DefaultCtxLoggerKey, l)
}

// From retrieves the logger stored in context if existing or returns NopLogger otherwise
func From(ctx context.Context) *Logger {
	l, ok := ctx.Value(DefaultCtxLoggerKey).(*Logger)
	if !ok {
		return NewNop()
	}
	return l
}

// WithFields adds all passed in zap fields to the Logger stored in ctx and overwrites it for further use
func WithFields(ctx context.Context, fields ...zapcore.Field) context.Context {
	l := From(ctx).WithFields(fields...)
	return WithLogger(ctx, l)
}

// WithFieldsOverwrite adds all passed in zap fields to the Logger stored in ctx and overwrites it for further use
// WARNING: This might kill thread safety - Experimental and bad practice - DO NOT USE!
func WithFieldsOverwrite(ctx context.Context, fields ...zapcore.Field) *Logger {
	l := From(ctx)
	n := l.WithFields(fields...)
	*l = *n
	return l
}

// New Logger sentry instance
func New(dsn string, debug, local bool) (*Logger, error) {
	sentry, err := raven.New(dsn)
	if err != nil {
		return nil, err
	}

	var cores []zapcore.Core
	if !debug {
		cores = append(cores, zapsentry.NewCore(zapcore.ErrorLevel, sentry))
	}
	if local {
		cores = append(cores, buildConsoleLogger(debug))
	} else {
		var stdr *zap.Logger
		if debug {
			stdr, err = zapdriver.NewProduction()
			if err != nil {
				return nil, err
			}
		} else {
			stdr, err = zapdriver.NewDevelopment()
			if err != nil {
				return nil, err
			}
		}
		cores = append(cores, stdr.Core())
	}

	logger := zap.New(zapcore.NewTee(cores...)).WithOptions(
		zap.AddCaller(),
		zap.AddStacktrace(zap.ErrorLevel),
	)

	return &Logger{
		Logger: logger,
		Sentry: sentry,

		dsn:   dsn,
		debug: debug,
		local: local,
		nop:   false,
	}, nil
}

// NewNop returns Logger with empty logging, tracing and ErrorReporting
func NewNop() *Logger {
	sentry, _ := raven.New("")
	logger := zap.NewNop()

	log := &Logger{
		Logger: logger,
		Sentry: sentry,
		nop:    true,
	}

	return log
}

// WithFields wrapper around zap.With
func (l *Logger) WithFields(fields ...zapcore.Field) *Logger {
	if l.nop {
		return l
	}
	log, err := New(l.dsn, l.debug, l.local)
	if err != nil {
		l.Error("creating new logger", zap.Error(err))
		return l
	}
	log.Sentry.SetRelease(l.Sentry.Release())
	log.Logger = l.Logger.With(fields...)
	return log
}

// IsNop returns the nop status of Logger (mainly for testing)
func (l *Logger) IsNop() bool {
	return l.nop
}

// To stores the current logger in the passed in context
func (l *Logger) To(ctx context.Context) context.Context {
	return WithLogger(ctx, l)
}

// WithRelease returns a new logger updating the internal sentry client with release info
// This should be the first change to the logger (before adding fields) as otherwise the change
// might not be persisted
func (l *Logger) WithRelease(info string) *Logger {
	if l.nop {
		return l
	}
	l.Sentry.SetRelease(info)
	log, err := New(l.dsn, l.debug, l.local)
	if err != nil {
		l.Error("creating new logger", zap.Error(err))
		return l
	}
	log.Sentry.SetRelease(info)
	log.Logger = l.Logger.With()
	return log
}

// NewSentryEncoder with dsn
func NewSentryEncoder(client *raven.Client) zapcore.Encoder {
	return newSentryEncoder(client)
}

func newSentryEncoder(client *raven.Client) *sentryEncoder {
	enc := &sentryEncoder{}
	enc.Sentry = client
	return enc
}

type sentryEncoder struct {
	zapcore.ObjectEncoder
	dsn    string
	Sentry *raven.Client
}

// Clone .
func (s *sentryEncoder) Clone() zapcore.Encoder {
	return newSentryEncoder(s.Sentry)
}

// EncodeEntry .
func (s *sentryEncoder) EncodeEntry(e zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	buf := buffer.NewPool().Get()
	if e.Level == zapcore.ErrorLevel {
		tags := s.Sentry.Tags
		if tags == nil {
			tags = make(map[string]string)
		}
		var err error
		for _, f := range fields {
			var tag string
			switch f.Type {
			case zapcore.StringType:
				tag = f.String
			case zapcore.Int16Type, zapcore.Int32Type, zapcore.Int64Type:
				tag = fmt.Sprintf("%v", f.Integer)
			case zapcore.ErrorType:
				err = f.Interface.(error)
			}
			tags[f.Key] = tag

		}
		if err == nil {
			s.Sentry.CaptureMessage(e.Message, tags)
			return buf, nil
		}
		s.Sentry.CaptureError(errors.Wrap(err, e.Message), tags)
	}
	return buf, nil
}

func (s *sentryEncoder) AddString(key, val string) {
	tags := s.Sentry.Tags
	if tags == nil {
		tags = make(map[string]string)
	}
	tags[key] = val
	s.Sentry.SetTagsContext(tags)
}

func (s *sentryEncoder) AddInt64(key string, val int64) {
	tags := s.Sentry.Tags
	if tags == nil {
		tags = make(map[string]string)
	}
	tags[key] = fmt.Sprint(val)
	s.Sentry.SetTagsContext(tags)
}

func buildConsoleLogger(dbg bool) zapcore.Core {
	debug := zapcore.Lock(os.Stdout)
	errors := zapcore.Lock(os.Stderr)

	config := zap.NewDevelopmentEncoderConfig()
	encoder := zapcore.NewConsoleEncoder(config)

	highPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.ErrorLevel
	})
	lowPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.InfoLevel && lvl < zapcore.ErrorLevel
	})
	debugPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.DebugLevel && lvl < zapcore.InfoLevel
	})

	cores := []zapcore.Core{
		zapcore.NewCore(encoder, errors, highPriority),
		zapcore.NewCore(encoder, debug, lowPriority),
	}

	if dbg {
		cores = append(cores, zapcore.NewCore(encoder, debug, debugPriority))
	}

	return zapcore.NewTee(cores...)
}

// buildLogger
func buildLogger(sentry *raven.Client, debug bool) *zap.Logger {
	highPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.ErrorLevel
	})
	lowPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.InfoLevel && lvl < zapcore.ErrorLevel
	})
	debugPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.DebugLevel && lvl < zapcore.InfoLevel
	})

	consoleDebugging := zapcore.Lock(os.Stdout)
	consoleErrors := zapcore.Lock(os.Stderr)
	consoleConfig := zap.NewDevelopmentEncoderConfig()
	consoleEncoder := zapcore.NewConsoleEncoder(consoleConfig)
	sentryEncoder := NewSentryEncoder(sentry)

	var core zapcore.Core
	cores := []zapcore.Core{
		zapcore.NewCore(consoleEncoder, consoleErrors, highPriority),
		zapcore.NewCore(consoleEncoder, consoleDebugging, lowPriority),
	}

	if debug {
		cores = append(cores, zapcore.NewCore(consoleEncoder, consoleDebugging, debugPriority))
	} else {
		cores = append(cores, zapcore.NewCore(sentryEncoder, consoleErrors, highPriority))
	}

	core = zapcore.NewTee(cores...)

	logger := zap.New(core)
	if debug {
		logger = logger.WithOptions(
			zap.AddCaller(),
			zap.AddStacktrace(zap.ErrorLevel),
		)
	} else {
		logger = logger.WithOptions(
			zap.AddStacktrace(zap.FatalLevel),
		)
	}
	return logger
}
