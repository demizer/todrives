package core

import (
	"io/ioutil"
	"logfmt"

	"github.com/Sirupsen/logrus"
	"github.com/davecgh/go-spew/spew"
)

// spd is used to dump in memory objects for debugging purposes.
var spd = spew.ConfigState{Indent: "\t"} //, DisableMethods: true}

// Log is the default logging object. By default, all output is discarded. Set Log.Out to std.Stdout to enable output. The
// level of the log output can also be set in this manner. See the documentation of the logrus package for other options.
var Log = &logrus.Logger{
	Out:       ioutil.Discard,
	Formatter: new(logfmt.TextFormatter),
	Hooks:     make(logrus.LevelHooks),
	Level:     logrus.InfoLevel,
}

// DoneSignalReceived indicates a process is shutting down.
type DoneSignalReceived bool

// Error satisfies the error interface.
func (s *DoneSignalReceived) Error() string {
	return "Done signal received"
}
