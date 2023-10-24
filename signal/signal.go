package signal

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// WaitForSigterm waits for either SIGTERM or SIGINT
//
// Returns the caught signal.
//
// It also prevent from program termination on SIGHUP signal,
// since this signal is frequently used for config reloading.
func WaitForSigterm() os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-ch
		if sig == syscall.SIGHUP {
			// Prevent from the program stop on SIGHUP
			continue
		}
		return sig
	}
}

// NewSigChan returns a channel, which is triggered on every SIGHUP, SIGTERM.
func NewSigChan() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM)
	return ch
}

// SelfSIGHUP sends SIGHUP signal to the current process.
func SelfSIGHUP() {
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		log.Panicf("FATAL: cannot send SIGHUP to itself: %s", err)
	}
}
