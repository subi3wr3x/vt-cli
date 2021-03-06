// Copyright © 2017 The VirusTotal CLI authors. All Rights Reserved.
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

package utils

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	vt "github.com/VirusTotal/vt-go"
	"github.com/briandowns/spinner"
	"github.com/plusvic/go-ansi"
)

// Coordinator coordinates the work of multiple instances of a Doer that run
// in parallel.
type Coordinator struct {
	Threads int
	Spinner *spinner.Spinner

	printingWg *sync.WaitGroup
	doerStates []DoerState
	resultsCh  chan string
}

// StringReader is the interface that wraps the ReadString method.
type StringReader interface {
	ReadString() (string, error)
}

// StringArrayReader is a wrapper around a slice of strings that implements
// the StringReader interface. Each time the ReadString method is called a
// string from the array is returned and the position is advanced by one. When
// all strings have been returned ReadString returns an io.EOF error.
type StringArrayReader struct {
	strings []string
	pos     int
}

// NewStringArrayReader creates a new StringArrayReader.
func NewStringArrayReader(strings []string) *StringArrayReader {
	return &StringArrayReader{strings: strings}
}

// ReadString reads one string from StringArrayReader. When all strings have
// been returned ReadString returns an io.EOF error.
func (sar *StringArrayReader) ReadString() (string, error) {
	if sar.pos == len(sar.strings) {
		return "", io.EOF
	}
	s := sar.strings[sar.pos]
	sar.pos++
	return s, nil
}

// StringIOReader is a wrapper around a bufio.Scanner that implements the
// StringReader interface.
type StringIOReader struct {
	scanner *bufio.Scanner
}

// NewStringIOReader creates a new StringIOReader.
func NewStringIOReader(r io.Reader) *StringIOReader {
	return &StringIOReader{scanner: bufio.NewScanner(r)}
}

// ReadString reads one string from StringIOReader. When all strings have
// been returned ReadString returns an io.EOF error.
func (sir *StringIOReader) ReadString() (string, error) {
	for sir.scanner.Scan() {
		s := strings.TrimSpace(sir.scanner.Text())
		if s != "" {
			return s, nil
		}
	}
	return "", io.EOF
}

// FilteredStringReader filters a StringReader returning only the strings that
// match a given regular expression.
type FilteredStringReader struct {
	r  StringReader
	re *regexp.Regexp
}

// NewFilteredStringReader creates a new FilteredStringReader that reads strings
// from r and return only those that match re.
func NewFilteredStringReader(r StringReader, re *regexp.Regexp) *FilteredStringReader {
	return &FilteredStringReader{r: r, re: re}
}

// ReadString reads strings from the the underlying StringReader and returns
// the first one that matches the regular expression specified while creating
// the FilteredStringReader. If no more strings can be read err is io.EOF.
func (f *FilteredStringReader) ReadString() (s string, err error) {
	for s, err = f.r.ReadString(); s != "" || err == nil; s, err = f.r.ReadString() {
		if f.re.MatchString(s) {
			return s, err
		}
	}
	return s, err
}

// DoerState represents the current state of a Doer.
type DoerState struct {
	Progress string
}

// Doer is the interface that must be implemented for any type to be used with
// DoWithStringsFromReader and DoWithStringsFromChannel.
type Doer interface {
	Do(interface{}, *DoerState) string
}

// NewCoordinator creates a new instance of Coordinator.
func NewCoordinator(threads int) *Coordinator {
	return &Coordinator{Threads: threads}
}

// EnableSpinner activates an animation while the coordinator is waiting.
func (c *Coordinator) EnableSpinner() {
	c.Spinner = spinner.New(spinner.CharSets[6], 250*time.Millisecond)
	c.Spinner.Color("green")
	c.Spinner.Suffix = " wait..."
}

// DoWithStringsFromReader calls the Do of a type implementing the Doer
// interface with strings read from a StringReader. The doer's Do method is
// called once for each string, and this function doesn't exit until the
// StringReader returns an empty string.
func (c *Coordinator) DoWithStringsFromReader(doer Doer, reader StringReader) {
	ch := make(chan interface{})
	go func() {
		for s, err := reader.ReadString(); s != "" || err == nil; s, err = reader.ReadString() {
			ch <- s
		}
		close(ch)
	}()
	c.DoWithItemsFromChannel(doer, ch)
}

// DoWithObjectsFromIterator calls the Do of a type implementing the Doer
// interface with the objects returned by a vt.Iterator. Objects returned by the
// iterator are put in a channel with a buffer size of bufferSize.
func (c *Coordinator) DoWithObjectsFromIterator(doer Doer, it *vt.Iterator, bufferSize int) {
	ch := make(chan interface{}, bufferSize)
	go func() {
		for it.Next() {
			ch <- it.Get()
		}
		close(ch)
	}()
	c.DoWithItemsFromChannel(doer, ch)
}

// DoWithItemsFromChannel calls the Do method of a type implementing the Doer
// interface with items read from a channel. This function doesn't exit until
// the channel is closed.
func (c *Coordinator) DoWithItemsFromChannel(doer Doer, ch <-chan interface{}) {

	c.resultsCh = make(chan string, c.Threads)
	c.doerStates = make([]DoerState, c.Threads)
	wg := &sync.WaitGroup{}

	for i := 0; i < c.Threads; i++ {
		wg.Add(1)
		go func(i int) {
			for arg := range ch {
				c.resultsCh <- doer.Do(arg, &c.doerStates[i])
				c.doerStates[i].Progress = ""
			}
			wg.Done()
		}(i)
	}

	c.printingWg = &sync.WaitGroup{}
	c.printingWg.Add(1)

	go c.printResults()

	wg.Wait()
	close(c.resultsCh)
	c.printingWg.Wait()
}

func (c *Coordinator) printResults() {
Loop:
	for {
		if c.Spinner != nil {
			c.Spinner.Start()
		}
		select {
		case res, ok := <-c.resultsCh:
			if !ok {
				break Loop
			}
			if c.Spinner != nil {
				c.Spinner.Stop()
			}
			ansi.Printf("%s", res)
			ansi.EraseInLine(0) // Clear to the end of the line.
			fmt.Println()
		default:
			// Print progress for pending workers
			lines := 0
			for _, ds := range c.doerStates {
				if ds.Progress != "" {
					ansi.Printf("%s", ds.Progress)
					ansi.EraseInLine(0) // Clear to the end of the line.
					fmt.Println()
					lines++
				}
			}
			time.Sleep(time.Millisecond * 250)
			if lines > 0 {
				// Move cursor up, to the line it was before printing worker's progress
				ansi.CursorPreviousLine(lines)
			}
		}
	}
	if c.Spinner != nil {
		c.Spinner.Stop()
	}
	c.printingWg.Done()
}
