package logger

import (
	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
)

type Logger struct {
	level int
	l     *logrus.Logger
	entry *logrus.Entry
}

func New(level int) *Logger {
	return &Logger{
		level: level,
		l:     logrus.StandardLogger(),
		entry: logrus.StandardLogger().WithFields(logrus.Fields{}),
	}
}

func (l *Logger) Enabled() bool {
	return true
}

func (l *Logger) Info(msg string, keysAndValues ...interface{}) {
	l.withValues(keysAndValues...).Debug(msg)
}

func (l *Logger) Error(err error, msg string, keysAndValues ...interface{}) {
	l.withValues(keysAndValues...).Errorf("%s: %v", msg, err)
}

func (l *Logger) V(level int) logr.Logger {
	if level <= l.level {
		return l.WithValues("v", level)
	}
	return logr.Discard()
}

func (l *Logger) WithValues(keysAndValues ...interface{}) logr.Logger {
	return &Logger{
		level: l.level,
		l:     l.l,
		entry: l.withValues(keysAndValues...),
	}
}

func (l *Logger) withValues(keysAndValues ...interface{}) *logrus.Entry {
	entry := l.entry
	for i := range keysAndValues {
		if i > 0 && i%2 == 0 {
			v := keysAndValues[i]
			k, ok := keysAndValues[i-1].(string)
			if !ok {
				continue
			}
			entry = entry.WithField(k, v)
		}
	}
	return entry
}

func (l *Logger) WithName(name string) logr.Logger {
	return l.WithValues("name", name)
}
