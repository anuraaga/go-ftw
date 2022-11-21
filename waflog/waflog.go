package waflog

import (
    "bytes"
    "errors"
    "fmt"
    "os"

    "github.com/anuraaga/go-ftw/config"
)

var errNoLogFile = errors.New("no log file supplied")

// NewFTWLogLines is the base struct for reading the log file
func NewFTWLogLines(opts ...FTWLogOption) (*FTWLogLines, error) {
    ll := &FTWLogLines{
        logFile:     nil,
        FileName:    config.FTWConfig.LogFile,
        StartMarker: nil,
        EndMarker:   nil,
    }

    // Loop through each option
    for _, opt := range opts {
        // Call the option giving the instantiated
        // *FTWLogOption as the argument
        opt(ll)
    }

    if err := ll.openLogFile(); err != nil {
        return nil, fmt.Errorf("cannot open log file: %w", err)
    }

    if config.FTWConfig.RunMode == config.DefaultRunMode && ll.logFile == nil {
        return nil, errNoLogFile
    }

    return ll, nil
}

// WithStartMarker sets the start marker for the log file
func WithStartMarker(marker []byte) FTWLogOption {
    return func(ll *FTWLogLines) {
        ll.StartMarker = bytes.ToLower(marker)
    }
}

// WithEndMarker sets the end marker for the log file
func WithEndMarker(marker []byte) FTWLogOption {
    return func(ll *FTWLogLines) {
        ll.EndMarker = bytes.ToLower(marker)
    }
}

// WithLogFile sets the log file to read
func WithLogFile(fileName string) FTWLogOption {
    return func(ll *FTWLogLines) {
        ll.FileName = fileName
    }
}

// Cleanup closes the log file
func (ll *FTWLogLines) Cleanup() error {
    if ll.logFile != nil {
        return ll.logFile.Close()
    }
    return nil
}

func (ll *FTWLogLines) openLogFile() error {
    // Using a log file is not required in cloud mode
    if config.FTWConfig.RunMode == config.DefaultRunMode {
        if ll.FileName != "" && ll.logFile == nil {
            var err error
            ll.logFile, err = os.Open(ll.FileName)
            return err
        }
    }
    return nil
}
