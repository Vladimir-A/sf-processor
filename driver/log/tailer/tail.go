//
// Copyright (C) 2022 IBM Corporation.
//
// Authors:
// Frederico Araujo <frederico.araujo@ibm.com>
// Teryl Taylor <terylt@ibm.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package tailer provides a class that is responsible for tailing log files
// and extracting new log lines to be passed into the virtual machines.
// Adapted from https://github.com/google/mtail/tree/main/internal
package tailer

import (
	"context"
	"errors"
	"expvar"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/sysflow-telemetry/sf-apis/go/logger"
	"github.com/sysflow-telemetry/sf-processor/driver/log/logline"
	"github.com/sysflow-telemetry/sf-processor/driver/log/tailer/logstream"
	"github.com/sysflow-telemetry/sf-processor/driver/log/waker"
)

// logCount records the number of logs that are being tailed.
var logCount = expvar.NewInt("log_count")

// Tailer polls the filesystem for log sources that match given
// `LogPathPatterns` and creates `LogStream`s to tail them.
type Tailer struct {
	ctx   context.Context
	wg    sync.WaitGroup // Wait for our subroutines to finish
	lines chan<- *logline.LogLine

	globPatternsMu     sync.RWMutex        // protects `globPatterns'
	globPatterns       map[string]struct{} // glob patterns to match newly created logs in dir paths against
	ignoreRegexPattern *regexp.Regexp

	socketPaths []string

	oneShot bool

	pollMu sync.Mutex // protects Poll()

	logstreamPollWaker waker.Waker                    // Used for waking idle logstreams
	logstreamsMu       sync.RWMutex                   // protects `logstreams`.
	logstreams         map[string]logstream.LogStream // Map absolte pathname to logstream reading that pathname.

	initDone chan struct{}
}

// Option configures a new Tailer.
type Option interface {
	apply(*Tailer) error
}

type niladicOption struct {
	applyfunc func(*Tailer) error
}

func (n *niladicOption) apply(t *Tailer) error {
	return n.applyfunc(t)
}

// OneShot puts the tailer in one-shot mode, where sources are read once from the start and then closed.
var OneShot = &niladicOption{func(t *Tailer) error { t.oneShot = true; return nil }}

// LogPatterns sets the glob patterns to use to match pathnames.
type LogPatterns []string

func (opt LogPatterns) apply(t *Tailer) error {
	for _, p := range opt {
		if err := t.AddPattern(p); err != nil {
			return err
		}
	}
	return nil
}

// IgnoreRegex sets the regular expression to use to filter away pathnames that match the LogPatterns glob.
type IgnoreRegex string

func (opt IgnoreRegex) apply(t *Tailer) error {
	return t.SetIgnorePattern(string(opt))
}

// StaleLogGcWaker triggers garbage collection runs for stale logs in the tailer.
func StaleLogGcWaker(w waker.Waker) Option {
	return &staleLogGcWaker{w}
}

type staleLogGcWaker struct {
	waker.Waker
}

func (opt staleLogGcWaker) apply(t *Tailer) error {
	t.StartStaleLogstreamExpirationLoop(opt.Waker)
	return nil
}

// LogPatternPollWaker triggers polls on the filesystem for new logs that match the log glob patterns.
func LogPatternPollWaker(w waker.Waker) Option {
	return &logPatternPollWaker{w}
}

type logPatternPollWaker struct {
	waker.Waker
}

func (opt logPatternPollWaker) apply(t *Tailer) error {
	t.StartLogPatternPollLoop(opt.Waker)
	return nil
}

// LogstreamPollWaker wakes idle logstreams.
func LogstreamPollWaker(w waker.Waker) Option {
	return &logstreamPollWaker{w}
}

type logstreamPollWaker struct {
	waker.Waker
}

func (opt logstreamPollWaker) apply(t *Tailer) error {
	t.logstreamPollWaker = opt.Waker
	return nil
}

var ErrNoLinesChannel = errors.New("Tailer needs a lines channel")

// New creates a new Tailer.
func New(ctx context.Context, wg *sync.WaitGroup, lines chan<- *logline.LogLine, options ...Option) (*Tailer, error) {
	if lines == nil {
		return nil, ErrNoLinesChannel
	}
	t := &Tailer{
		ctx:          ctx,
		lines:        lines,
		initDone:     make(chan struct{}),
		globPatterns: make(map[string]struct{}),
		logstreams:   make(map[string]logstream.LogStream),
	}
	defer close(t.initDone)
	if err := t.SetOption(options...); err != nil {
		return nil, err
	}
	if len(t.globPatterns) == 0 && len(t.socketPaths) == 0 {
		logger.Info.Println("No patterns or sockets to tail, tailer done.")
		close(t.lines)
		return t, nil
	}
	// Set up listeners on every socket.
	for _, pattern := range t.socketPaths {
		if err := t.TailPath(pattern); err != nil {
			return nil, err
		}
	}
	// Guarantee all existing logs get tailed before we leave.  Also necessary
	// in case oneshot mode is active, the logs get read!
	if err := t.PollLogPatterns(); err != nil {
		return nil, err
	}
	// Setup for shutdown, once all routines are finished.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-t.initDone
		// We need to wait for context.Done() before we wait for the subbies
		// because we don't know how many are running at any point -- as soon
		// as t.wg.Wait begins the number of waited-on goroutines is fixed, and
		// we may end up leaking a LogStream goroutine and it'll try to send on
		// a closed channel as a result.  But in tests and oneshot, we want to
		// make sure the whole log gets read so we can't wait on context.Done
		// here.
		if !t.oneShot {
			<-t.ctx.Done()
		}
		t.wg.Wait()
		close(t.lines)
	}()
	return t, nil
}

var ErrNilOption = errors.New("nil option supplied")

// SetOption takes one or more option functions and applies them in order to Tailer.
func (t *Tailer) SetOption(options ...Option) error {
	for _, option := range options {
		if option == nil {
			return ErrNilOption
		}
		if err := option.apply(t); err != nil {
			return err
		}
	}
	return nil
}

var ErrUnsupportedURLScheme = errors.New("unsupported URL scheme")

// AddPattern adds a pattern to the list of patterns to filter filenames against.
func (t *Tailer) AddPattern(pattern string) error {
	u, err := url.Parse(pattern)
	if err != nil {
		return err
	}

	path := pattern
	switch u.Scheme {
	default:
		logger.Info.Printf("%v: %q in path pattern %q, treating as path", ErrUnsupportedURLScheme, u.Scheme, pattern)
	case "unix", "unixgram", "tcp", "udp":
		// Keep the scheme.
		logger.Info.Printf("AddPattern: socket %q", pattern)
		t.socketPaths = append(t.socketPaths, pattern)
		return nil
	case "", "file":
		path = u.Path
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		logger.Info.Printf("Couldn't canonicalize path %q: %s", u.Path, err)
		return err
	}
	logger.Info.Printf("AddPattern: file %q", absPath)
	t.globPatternsMu.Lock()
	t.globPatterns[absPath] = struct{}{}
	t.globPatternsMu.Unlock()
	return nil
}

func (t *Tailer) Ignore(pathname string) bool {
	absPath, err := filepath.Abs(pathname)
	if err != nil {
		logger.Info.Printf("Couldn't get absolute path for %q: %s", pathname, err)
		return true
	}
	fi, err := os.Stat(absPath)
	if err != nil {
		logger.Info.Printf("Couldn't stat path %q: %s", pathname, err)
		return true
	}
	if fi.Mode().IsDir() {
		logger.Info.Printf("ignore path %q because it is a folder", pathname)
		return true
	}
	return t.ignoreRegexPattern != nil && t.ignoreRegexPattern.MatchString(fi.Name())
}

func (t *Tailer) SetIgnorePattern(pattern string) error {
	if len(pattern) == 0 {
		return nil
	}
	logger.Info.Printf("Set filename ignore regex pattern %q", pattern)
	ignoreRegexPattern, err := regexp.Compile(pattern)
	if err != nil {
		logger.Info.Printf("Couldn't compile regex %q: %s", pattern, err)
		fmt.Printf("error: %v\n", err)
		return err
	}
	t.ignoreRegexPattern = ignoreRegexPattern
	return nil
}

// TailPath registers a filesystem pathname to be tailed.
func (t *Tailer) TailPath(pathname string) error {
	t.logstreamsMu.Lock()
	defer t.logstreamsMu.Unlock()
	if l, ok := t.logstreams[pathname]; ok {
		if !l.IsComplete() {
			logger.Info.Printf("already got a logstream on %q", pathname)
			return nil
		}
		logCount.Add(-1) // Removing the current entry before re-adding.
		logger.Info.Printf("Existing logstream is finished, creating a new one.")
	}
	l, err := logstream.New(t.ctx, &t.wg, t.logstreamPollWaker, pathname, t.lines, t.oneShot)
	if err != nil {
		return err
	}
	if t.oneShot {
		logger.Info.Printf("Starting oneshot read at startup of %q", pathname)
		l.Stop()
	}
	t.logstreams[pathname] = l
	logger.Info.Printf("Tailing %s", pathname)
	logCount.Add(1)
	return nil
}

// ExpireStaleLogstreams removes logstreams that have had no reads for 1h or more.
func (t *Tailer) ExpireStaleLogstreams() error {
	t.logstreamsMu.Lock()
	defer t.logstreamsMu.Unlock()
	for _, v := range t.logstreams {
		if time.Since(v.LastReadTime()) > (time.Hour * 24) {
			v.Stop()
		}
	}
	return nil
}

// StartStaleLogstreamExpirationLoop runs a permanent goroutine to expire stale logstreams.
func (t *Tailer) StartStaleLogstreamExpirationLoop(waker waker.Waker) {
	if waker == nil {
		logger.Info.Println("Log handle expiration disabled")
		return
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		<-t.initDone
		if t.oneShot {
			logger.Info.Println("No gc loop in oneshot mode.")
			return
		}
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-waker.Wake():
				if err := t.ExpireStaleLogstreams(); err != nil {
					logger.Info.Println(err)
				}
			}
		}
	}()
}

// StartLogPatternPollLoop runs a permanent goroutine to poll for new log files.
func (t *Tailer) StartLogPatternPollLoop(waker waker.Waker) {
	if waker == nil {
		logger.Info.Println("Log pattern polling disabled")
		return
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		<-t.initDone
		if t.oneShot {
			logger.Info.Println("No polling loop in oneshot mode.")
			return
		}
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-waker.Wake():
				if err := t.Poll(); err != nil {
					logger.Info.Println(err)
				}
			}
		}
	}()
}

func (t *Tailer) PollLogPatterns() error {
	t.globPatternsMu.RLock()
	defer t.globPatternsMu.RUnlock()
	for pattern := range t.globPatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		logger.Info.Printf("glob matches: %v", matches)
		for _, pathname := range matches {
			if t.Ignore(pathname) {
				continue
			}
			absPath, err := filepath.Abs(pathname)
			if err != nil {
				logger.Info.Printf("Couldn't get absolute path for %q: %s", pathname, err)
				continue
			}
			logger.Info.Printf("watched path is %q", absPath)
			if err := t.TailPath(absPath); err != nil {
				logger.Info.Println(err)
			}
		}
	}
	return nil
}

// PollLogStreamsForCompletion looks at the existing paths and checks if they're already
// complete, removing it from the map if so.
func (t *Tailer) PollLogStreamsForCompletion() error {
	t.logstreamsMu.Lock()
	defer t.logstreamsMu.Unlock()
	for name, l := range t.logstreams {
		if l.IsComplete() {
			logger.Info.Printf("%s is complete", name)
			delete(t.logstreams, name)
			logCount.Add(-1)
			continue
		}
	}
	return nil
}

func (t *Tailer) Poll() error {
	t.pollMu.Lock()
	defer t.pollMu.Unlock()
	for _, f := range []func() error{t.PollLogPatterns, t.PollLogStreamsForCompletion} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}
