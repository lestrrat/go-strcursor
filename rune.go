package strcursor

import (
	"errors"
	"io"
	"unicode/utf8"
)

// RuneCursor is a cursor for consumers that are interested in series of
// runes (not bytes)
type RuneCursor struct {
	buf      []byte    // scratch bufer, read in from the io.Reader
	buflen   int       // size of scratch buffer
	bufpos   int			 // amount consumed within the scratch buffer
	column   int       // column number
	in       io.Reader // input source
	lineno   int			 // line number
	nread    int       // number of bytes consumed so far
	rabuf    *runebuf  // Read-ahead buffer.
	rabuflen int       // Number of runes in read-ahead buffer
}

type runebuf struct {
	val   rune
	width int
	next  *runebuf
}

// NewRuneCursor creates a cursor that deals exclusively with runes
func NewRuneCursor(in io.Reader, nn ...int) *RuneCursor {
	var n int
	if len(nn) > 0 {
		n = nn[0]
	}
	// This buffer is used when reading from the underlying io.Reader.
	// It is necessary to read from the io.Reader because otherwise
	// we can't call utf8.DecodeRune on it
	if n <= 0 {
		// by default, read up to 40 bytes = maximum 10 runes worth of data
		n = 40
	}

	buf := make([]byte, n)
	return &RuneCursor{
		buf:    buf,
		buflen: n,
		bufpos: n, // set to maximum to force filling up the bufer on first read
		column: 1,
		in:     in,
		lineno: 1,
		nread:  0,
		rabuf:  nil,
	}
}

// decode the contents of c.buf into runes, and append to the
// read-ahead rune buffer
func (c *RuneCursor) decodeIntoRuneBuffer() error {
	var last *runebuf
	for c.bufpos < c.buflen {
		r, w := utf8.DecodeRune(c.buf[c.bufpos:])
		if r == utf8.RuneError {
			return errors.New("failed to decode")
		}
		c.bufpos += w
		c.rabuflen++
		cur := &runebuf{
			val:   r,
			width: w,
		}
		if last == nil {
			c.rabuf = cur
		} else {
			last.next = cur
		}
		last = cur
	}
	return nil
}

func (c *RuneCursor) fillRuneBuffer(n int) error {
	// Check if we have a read-ahead rune buffer
	if c.rabuflen >= n {
		return nil
	}

	// Fill the buffer until we have n runes. However, make sure to
	// detect if we have a failure loop
	prevrabuflen := c.rabuflen
	for {
		// do we have the underlying byte buffer? if we have at least 1 byte,
		// we may be able to decode it
		c.decodeIntoRuneBuffer()
		// we still have a chance to read from the underlying source
		// and succeed in decoding, so we won't return here, even if there
		// was an error

		// we got enough. return success
		if c.rabuflen >= n {
			return nil
		}

		// Hmm, still didn't read anything? try reading from the underlying
		// io.Reader.
		if c.bufpos < c.buflen {
			// first, rescue the remaining bytes, if any. only do the copying
			// when we have something left to consume in the buffer
			copy(c.buf, c.buf[c.bufpos:])
		}

		// reset bufpos.
		if c.bufpos > c.buflen {
			// If bufpos is for some reason > c.buflen, just set it to 0
			c.bufpos = 0
		} else {
			// Otherwise, the remaining bytes up to buflen is the content
			// that is yet to be consumed
			c.bufpos = c.buflen - c.bufpos
		}
		n, err := c.in.Read(c.buf[c.bufpos:])
		if n == 0 && err != nil {
			// Oh, we're done. really done.
			c.buf = []byte{}
			c.buflen = 0
			return err
		}
		c.buflen = n
		// well, we read something. see if we can fill the rune buffer
		c.decodeIntoRuneBuffer()

		// let the next section handle the error
		if prevrabuflen == c.rabuflen {
			c.buf = []byte{}
			c.buflen = 0
			return errors.New("failed to fill read buffer")
		}

		prevrabuflen = c.rabuflen
	}

	return errors.New("unrecoverable error")
}

// Done returns true if there are no more runes left.
func (c *RuneCursor) Done() bool {
	if err := c.fillRuneBuffer(1); err != nil {
		return true
	}
	return false
}

// Cur returns the first rune and consumes it.
func (c *RuneCursor) Cur() rune {
	if err := c.fillRuneBuffer(1); err != nil {
		return utf8.RuneError
	}

	// Okay, we got something. Pop off the stack, and we're done
	head := c.rabuf
	c.Advance(1)
	return head.val
}

// Peek returns the first rune without consuming it.
func (c *RuneCursor) Peek() rune {
	return c.PeekN(1)
}

// PeekN returns the n-th rune without consuming it.
func (c *RuneCursor) PeekN(n int) rune {
	if err := c.fillRuneBuffer(n); err != nil {
		return utf8.RuneError
	}

	cur := c.rabuf
	for i := 1; i < n; i++ {
		cur = cur.next
	}
	return cur.val
}

// Advance advances the cursor n runes
func (c *RuneCursor) Advance(n int) error {
	head := c.rabuf
	for i := 0; i < n; i++ {
		if head == nil {
			return errors.New("failed to pop enough runes")
		}
		c.nread += head.width
		if head.val == '\n' {
			c.lineno++
			c.column = 1
		} else {
			c.column++
		}
		head = head.next
		c.rabuflen--
	}
	c.rabuf = head
	return nil
}

func (c *RuneCursor) hasPrefix(s string, n int, consume bool) bool {
	// First, make sure we have enough read ahead buffer
	if err := c.fillRuneBuffer(n); err != nil {
		return false
	}

	nl := 0
	col := c.column
	for cur := c.rabuf; cur != nil; cur = cur.next {
		r, w := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError {
			return false
		}
		s = s[w:]
		if cur.val != r {
			return false
		}
		if r == '\n' {
			nl++
			col = 1
		} else {
			col++
		}

		if len(s) == 0 {
			// match! if we have the consume flag set, change the pointers
			if consume {
				c.rabuf = cur.next
				c.column = col
				c.lineno += nl
			}
			return true
		}
	}
	return false
}

// HasPrefix takes a string returns true if the rune buffer contains
// the exact sequence of runes. This method does NOT consume upon a match
func (c *RuneCursor) HasPrefix(s string) bool {
	n := utf8.RuneCountInString(s)
	return c.hasPrefix(s, n, false)
}

// Consume takes a string and advances the cursor that many runes
// if the rune buffer contains the exact sequence of runes
func (c *RuneCursor) Consume(s string) bool {
	n := utf8.RuneCountInString(s)
	return c.hasPrefix(s, n, true)
}

// LineNumber returns the current line number
func (c *RuneCursor) LineNumber() int {
	return c.lineno
}

// Column returns the current column number
func (c *RuneCursor) Column() int {
	return c.column
}