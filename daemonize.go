// Copyright 2015 Google Inc. All Rights Reserved.
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

// Helper code for starting a daemon process.
//
// This package provides the convenience for writing a tool that spins itself
// off as a daemon process. The daemon should be able to communicate status
// to the user while it starts up, causing the invoking process to
// exit in success or failure only when it is clear whether the daemon has
// sucessfully started (which it signal using SignalOutcome).
package daemonize

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/kardianos/osext"
)

// The name of an environment variable used to communicate a unix socket
// set up by Run to the daemon process. Gob encoding is used to communicate
// back to Run.
const envVar = "DAEMONIZE_STATUS_ADDR"

// A message containing logging output while starting the daemon.
type logMsg struct {
	Msg []byte
}

// A message indicating the outcome of starting the daemon. The receiver
// ignores further messages.
type outcomeMsg struct {
	Successful bool

	// Meaningful only if !Successful.
	ErrorMsg string
}

// A gob encoder to write back to the parent process
var gGobEncoder *gob.Encoder

func init() {
	gob.Register(logMsg{})
	gob.Register(outcomeMsg{})

	// Is the environment variable set?
	addr, ok := os.LookupEnv(envVar)
	if !ok {
		return
	}

	// Parse the file descriptor.
	conn, err := net.Dial("unix", addr)
	if err != nil {
		log.Fatalf("Couldn't connect to %v : %v", addr, err)
	}

	gGobEncoder = gob.NewEncoder(conn)
}

// Send the supplied message as an interface{}, matching the decoder.
func sendMsg(msg interface{}) (err error) {
	err = gGobEncoder.Encode(&msg)
	return
}

// For use by the daemon: signal an outcome back to Run in the invoking tool,
// causing it to return. Do nothing if the process wasn't invoked with Run.
func SignalOutcome(outcome error) (err error) {
	// Is there anything to do?
	if gGobEncoder == nil {
		return
	}

	// Write out the outcome.
	msg := &outcomeMsg{
		Successful: outcome == nil,
	}

	if !msg.Successful {
		msg.ErrorMsg = outcome.Error()
	}

	err = sendMsg(msg)

	return
}

// An io.Writer that sends logMsg messages over gGobEncoder.
type logMsgWriter struct {
}

func (w *logMsgWriter) Write(p []byte) (n int, err error) {
	msg := &logMsg{
		Msg: p,
	}

	err = sendMsg(msg)
	if err != nil {
		return
	}

	n = len(p)
	return
}

// Invoke the daemon with the supplied arguments, waiting until it successfully
// starts up or reports that is has failed. Write status updates while starting
// into the supplied writer (which may be nil for silence). Return nil only if
// it starts successfully.
func Run(args []string, status io.Writer) (err error) {
	if _, envAddr := os.LookupEnv(envVar); envAddr {
		return errors.New("Can't nest calls to daemonize.Run")
	}
	if status == nil {
		status = ioutil.Discard
	} else {
		// wrap stdout and stderr so that we don't write to those once the parent process exits
		status = &passThroughWriter{status}
	}

	// Set up the socket that we will hand to the daemon.
	dir, err := ioutil.TempDir("", "daemonize")
	if err != nil {
		log.Fatal("Error starting server: ", err)
	}
	defer os.RemoveAll(dir)

	addr := filepath.Join(dir, "daemon.sock")
	listener, err := net.Listen("unix", addr)
	if err != nil {
		log.Fatal("Error starting server: ", err)
	}
	defer listener.Close()

	os.Setenv(envVar, addr)

	// Attempt to start the daemon process. If we encounter an error in so doing,
	// write it to the channel.
	startProcessErr := make(chan error, 1)
	go func() {
		err := startProcess(args, status)
		if err != nil {
			startProcessErr <- err
		}
	}()

	// Read communication from the daemon from the pipe, writing nil into the
	// channel only if the startup succeeds.
	readFromProcessOutcome := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Fatal("Error listening for connection:", err)
			}
			readFromProcessOutcome <- readFromProcess(conn, status)
		}
	}()

	// Wait for a result from one of the above.
	select {
	case err = <-startProcessErr:
		err = fmt.Errorf("startProcess: %v", err)
		return

	case err = <-readFromProcessOutcome:
		if err == nil {
			// All is good.
			return
		}

		err = fmt.Errorf("readFromProcess: %v", err)
		return
	}
}

// Start the daemon process and do
// not wait for it to return.
func startProcess(args []string, status io.Writer) error {

	// TODO: drop osext dependency once minmum required go verison is 1.8
	// path, err := os.Executable()
	path, err := osext.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(path)
	cmd.Stdout = status
	cmd.Args = append(cmd.Args, args...)

	// Call setsid after forking in order to avoid being killed when the user
	// logs out.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Start. Clean up in the background, ignoring errors.
	err = cmd.Start()
	go cmd.Wait()

	return err
}

// Process communication from a daemon subprocess. Write log messages to the
// supplied writer (which must be non-nil). Return nil only if the startup
// succeeds.
func readFromProcess(
	r io.Reader,
	status io.Writer) (err error) {
	decoder := gob.NewDecoder(r)

	for {
		// Read a message.
		var msg interface{}
		err = decoder.Decode(&msg)
		if err != nil {
			err = fmt.Errorf("Decode: %v", err)
			return
		}

		// Handle the message.
		switch msg := msg.(type) {
		case logMsg:
			_, err = status.Write(msg.Msg)
			if err != nil {
				err = fmt.Errorf("status.Write: %v", err)
				return
			}

		case outcomeMsg:
			if msg.Successful {
				return
			}

			err = fmt.Errorf("sub-process: %s", msg.ErrorMsg)
			return

		default:
			err = fmt.Errorf("Unhandled message type: %T", msg)
			return
		}
	}
}

// this is used to avoid assigning stdout or stderr directly to os/exec.Cmd's Stdout, which would produce output
// even after the launching process has ended
type passThroughWriter struct {
	io.Writer
}

func (w *passThroughWriter) Write(p []byte) (n int, err error) {
	return w.Writer.Write(p)
}
