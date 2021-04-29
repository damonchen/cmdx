package cmdx

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type TimeoutError struct {
	timeout int
}

func (t TimeoutError) Error() string {
	return fmt.Sprintf("%ds timeout", t.timeout)
}

func (t TimeoutError) Timeout() int {
	return t.timeout
}

type closeFunc func()

type writerWrapper struct {
	w io.Writer
}

func newWriterWrapper(w io.Writer) *writerWrapper {
	return &writerWrapper{w: w}
}

// Write write
func (w *writerWrapper) Write(b []byte) (int, error) {
	return w.w.Write(b)
}

type tickWriter struct {
	mu     sync.Mutex
	tick   chan struct{}
	closed bool

	w io.Writer
}

func newTickWriter(tick chan struct{}, w io.Writer) *tickWriter {
	return &tickWriter{
		tick: tick,
		w:    w,
	}
}

func (t *tickWriter) triggerTick() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.tick <- struct{}{} // may closed by
	}
}

// Write write
func (t *tickWriter) Write(b []byte) (int, error) {
	t.triggerTick()
	return t.w.Write(b)
}

// Close close
func (t *tickWriter) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true
	return nil
}

// Tick tick
func (t *tickWriter) Tick() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	tick := t.tick
	return tick
}

// CmdStatus cmd status
type CmdStatus int

// String cmd status string
func (c CmdStatus) String() string {
	switch c {
	case CmdInit:
		return "CmdInit"
	case CmdRunning:
		return "CmdRunning"
	case CmdTimeout:
		return "CmdTimeout"
	case CmdCompleted:
		return "CmdCompleted"
	case CmdCancel:
		return "CmdCancel"
	}
	return ""
}

const (
	// init status
	CmdInit CmdStatus = iota
	// running status
	CmdRunning
	// timeout status
	CmdTimeout
	// completed status
	CmdCompleted
	// cancel status
	CmdCancel
)

// Cmd cmd
type Cmd struct {
	//*exec.Cmd
	name       string
	ctx        context.Context
	cancelFunc context.CancelFunc

	stdin            io.Reader
	stdout           io.Writer
	stdoutClose      closeFunc
	stdoutTickWriter *tickWriter

	stderr           io.Writer
	stderrClose      closeFunc
	stderrTickWriter *tickWriter

	startTime time.Time
	endTime   time.Time

	args         []string
	dir          string
	env          []string
	timeout      int
	killChildren bool
	tickSignal   bool

	cmd *exec.Cmd

	// 当结束或者超时的时候发出
	mu     sync.Mutex // protects following fields
	tick   chan struct{}
	closed bool
	status CmdStatus
}

// CmdOption cmd option
type CmdOption func(c *Cmd)

// WithStdin with stdin
func WithStdin(reader io.Reader) CmdOption {
	return func(c *Cmd) {
		c.stdin = reader
	}
}

// WithStdout with stderr
func WithStdout(writer io.Writer) CmdOption {
	return func(c *Cmd) {
		c.stdout = writer
	}
}

// WithStdoutCloser write closer
func WithStdoutCloser(writeCloser io.WriteCloser) CmdOption {
	return func(c *Cmd) {
		c.stdout = writeCloser
		c.stdoutClose = func() {
			writeCloser.Close()
		}
	}
}

// WithStderr with stderr
func WithStderr(writer io.Writer) CmdOption {
	return func(c *Cmd) {
		c.stderr = writer
	}
}

// WithStdoutCloser stderr closer
func WithStderrCloser(writeCloser io.WriteCloser) CmdOption {
	return func(c *Cmd) {
		c.stderr = writeCloser
		c.stderrClose = func() {
			writeCloser.Close()
		}
	}
}

// WithArgs with args
func WithArgs(args ...string) CmdOption {
	return func(c *Cmd) {
		c.args = args
	}
}

// WithEnvs with envs
func WithEnvs(envs []string) CmdOption {
	return func(c *Cmd) {
		c.env = envs
	}
}

// WithDir with dir
func WithDir(dir string) CmdOption {
	return func(c *Cmd) {
		c.dir = dir
	}
}

// WithTimeout time unit second
func WithTimeout(timeout int) CmdOption {
	return func(c *Cmd) {
		c.timeout = timeout
	}
}

// WithKillChildren with kill chidren
func WithKillChildren() CmdOption {
	return func(c *Cmd) {
		c.killChildren = true
	}
}

func WithTickSignal() CmdOption {
	return func(c *Cmd) {
		c.tickSignal = true
	}
}

// NewCmd new cmd
func NewCmd(name string, opts ...CmdOption) *Cmd {
	cmd := &Cmd{
		status: CmdInit,
		ctx:    context.Background(),
		name:   name,
	}

	for _, opt := range opts {
		opt(cmd)
	}

	return cmd
}

func (c *Cmd) completed() {
	c.mu.Lock()
	c.closed = true
	if c.tick != nil {
		close(c.tick)
	}
	c.mu.Unlock()

	if c.stdoutTickWriter != nil {
		_ = c.stdoutTickWriter.Close()
	}
	if c.stderrTickWriter != nil {
		_ = c.stderrTickWriter.Close()
	}

	if c.stdoutClose != nil {
		c.stdoutClose()
	}
	if c.stderrClose != nil {
		c.stderrClose()
	}

	if c.cancelFunc != nil {
		c.cancelFunc()
	}
}

// Done done, you must not cache the done chan
func (c *Cmd) Done() <-chan struct{} {
	return c.ctx.Done()
}

// Tick tick chan, when data write to stdout or stderr, it will a tick for outer process data
func (c *Cmd) Tick() <-chan struct{} {
	if !c.tickSignal {
		return nil
	}

	c.mu.Lock()
	t := c.tick
	c.mu.Unlock()
	return t
}

// Cmd *exec.Cmd
func (c *Cmd) Cmd() *exec.Cmd {
	return c.cmd
}

// Cancel cancel command, will kill the cmd process
func (c *Cmd) Cancel() {
	c.mu.Lock()
	if c.status == CmdRunning && c.cancelFunc != nil {
		c.cancelFunc()
		c.status = CmdCancel
	}
	c.mu.Unlock()
}

// Status status of cmd
func (c *Cmd) Status() CmdStatus {
	c.mu.Lock()
	s := c.status
	c.mu.Unlock()
	return s
}

func (c *Cmd) Pid() int {
	if c.status == CmdInit {
		return -1
	}
	return c.cmd.ProcessState.Pid()
}

// Time total time of cmd run, return 0 if not start running
func (c *Cmd) Time() time.Duration {
	if c.status <= CmdRunning {
		return 0
	}
	return c.endTime.Sub(c.startTime)
}

func (c *Cmd) newCmd() *exec.Cmd {
	// it will be sure to kill all the child and grand children processes when using kill function
	if c.timeout != 0 {
		timeout := time.Second * time.Duration(c.timeout)
		ctx, cancelFunc := context.WithTimeout(c.ctx, timeout)
		c.cancelFunc = cancelFunc
		c.ctx = ctx
	}

	if c.tickSignal {
		c.tick = make(chan struct{})
	}

	cmd := exec.CommandContext(c.ctx, c.name, c.args...)

	if c.killChildren {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if c.stdout != nil {
		if c.tickSignal {
			c.stdoutTickWriter = newTickWriter(c.tick, c.stdout)
			cmd.Stdout = c.stdoutTickWriter
		} else {
			cmd.Stdout = c.stdout
		}
	}

	if c.stderr != nil {
		if c.tickSignal {
			c.stderrTickWriter = newTickWriter(c.tick, c.stderr)
			cmd.Stderr = c.stderrTickWriter
		} else {
			cmd.Stderr = c.stderr
		}
	}
	cmd.Stdin = c.stdin
	return cmd
}

// Run run the command, synchronize
func (c *Cmd) Run() error {
	cmd := c.newCmd()
	c.cmd = cmd

	defer c.completed()
	err := cmd.Start()
	if err != nil {
		return err
	}

	c.mu.Lock()
	c.status = CmdRunning
	c.mu.Unlock()

	c.startTime = time.Now()

	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = cmd.Wait()
		done <- struct{}{}
	}()

	select {
	case <-done:
		// 正常结束
		c.mu.Lock()
		if c.status == CmdRunning {
			c.status = CmdCompleted
		}
		c.mu.Unlock()
		err = runErr
	case <-c.ctx.Done():
		// 超时结束
		c.mu.Lock()
		if c.status == CmdRunning {
			c.status = CmdTimeout
		}
		c.mu.Unlock()
		err = &TimeoutError{timeout: c.timeout}
	}
	c.endTime = time.Now()
	return err
}

// NewPipeCmd new pipe cmd
func NewPipeCmd(name string, opts ...CmdOption) (*Cmd, io.ReadCloser, io.ReadCloser) {
	stdoutReader, stdout := io.Pipe()
	stderrReader, stderr := io.Pipe()

	cmd := NewCmd(name, opts...)

	cmd.stdout = stdout
	cmd.stdoutClose = func() {
		stdout.Close()
	}

	cmd.stderr = stderr
	cmd.stderrClose = func() {
		stderr.Close()
	}

	return cmd, stdoutReader, stderrReader
}

// NewCombinedPipeCmd new combined pipe cmd
func NewCombinedPipeCmd(name string, opts ...CmdOption) (*Cmd, io.ReadCloser) {
	stdoutReader, stdout := io.Pipe()
	cmd := NewCmd(name, opts...)
	cmd.stdout = stdout
	cmd.stdoutClose = func() {
		stdout.Close()
	}

	return cmd, stdoutReader
}
