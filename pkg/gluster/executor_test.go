package gluster

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		host    string
		args    []string
		wantErr bool
	}{
		{name: "valid command", host: "gluster-1", args: []string{"gluster", "--mode=script", "--xml", "volume", "info"}},
		{name: "valid command with an empty argument", host: "gluster-1", args: []string{"gluster", ""}},
		{name: "valid command with an IPv4 host", host: "10.0.0.13", args: []string{"gluster"}},
		{name: "valid command with a qualified host name", host: "gluster-1.example.com.", args: []string{"gluster"}},
		{name: "valid command with non ASCII arguments", host: "gluster-1", args: []string{"gluster", "卷", "--option=a b"}},

		{name: "empty host", host: "", args: []string{"gluster"}, wantErr: true},
		{name: "host with a space", host: "gluster 1", args: []string{"gluster"}, wantErr: true},
		{name: "host with a tab", host: "gluster\t1", args: []string{"gluster"}, wantErr: true},
		{name: "host with a newline", host: "gluster\n1", args: []string{"gluster"}, wantErr: true},
		{name: "host with a control character", host: "gluster\x00", args: []string{"gluster"}, wantErr: true},
		{name: "host with at sign", host: "user@gluster-1", args: []string{"gluster"}, wantErr: true},
		{name: "host with a slash", host: "../gluster-1", args: []string{"gluster"}, wantErr: true},
		{name: "host with a backslash", host: `gluster\1`, args: []string{"gluster"}, wantErr: true},
		{name: "host that is not valid UTF-8", host: "gluster-\xff", args: []string{"gluster"}, wantErr: true},
		{name: "host longer than the limit", host: strings.Repeat("a", maxHostLength+1), args: []string{"gluster"}, wantErr: true},

		{name: "empty argument list", host: "gluster-1", args: nil, wantErr: true},
		{name: "program sh", host: "gluster-1", args: []string{"sh", "-c", "id"}, wantErr: true},
		{name: "program with an absolute path", host: "gluster-1", args: []string{"/usr/sbin/gluster"}, wantErr: true},
		{name: "program with a command separator", host: "gluster-1", args: []string{"gluster;id"}, wantErr: true},
		{name: "program with a leading space", host: "gluster-1", args: []string{" gluster"}, wantErr: true},
		{name: "program that is only whitespace", host: "gluster-1", args: []string{" "}, wantErr: true},
		{name: "program in upper case", host: "gluster-1", args: []string{"Gluster"}, wantErr: true},

		{name: "argument with a newline", host: "gluster-1", args: []string{"gluster", "a\nb"}, wantErr: true},
		{name: "argument with a carriage return", host: "gluster-1", args: []string{"gluster", "a\rb"}, wantErr: true},
		{name: "argument with a NUL byte", host: "gluster-1", args: []string{"gluster", "a\x00b"}, wantErr: true},
		{name: "argument with an escape character", host: "gluster-1", args: []string{"gluster", "a\x1bb"}, wantErr: true},
		{name: "argument with a delete character", host: "gluster-1", args: []string{"gluster", "a\x7fb"}, wantErr: true},
		{name: "argument that is not valid UTF-8", host: "gluster-1", args: []string{"gluster", "\xff\xfe"}, wantErr: true},
		{name: "argument with a control character in a later position", host: "gluster-1",
			args: []string{"gluster", "volume", "create\x01brick"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCommand(tc.host, tc.args)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateCommand(%q, %q) = %v, want nil", tc.host, tc.args, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateCommand(%q, %q) = nil, want an error", tc.host, tc.args)
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateCommand(%q, %q) = %v, want errors.Is(err, ErrInvalidCommand)", tc.host, tc.args, err)
			}
			if errors.Is(err, ErrConnect) {
				t.Fatalf("validateCommand(%q, %q) = %v, must not wrap ErrConnect", tc.host, tc.args, err)
			}
		})
	}
}

func TestValidateCommandRejectsLongCommand(t *testing.T) {
	t.Parallel()

	// A single argument just below and just above the limit of the quoted
	// command line, which includes the two quotes of the argument.
	atLimit := strings.Repeat("a", MaxCommandLength-len("'gluster' ")-2)
	if err := validateCommand("gluster-1", []string{"gluster", atLimit}); err != nil {
		t.Fatalf("validateCommand at the limit = %v, want nil", err)
	}
	tooLong := atLimit + "a"
	err := validateCommand("gluster-1", []string{"gluster", tooLong})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("validateCommand above the limit = %v, want ErrInvalidCommand", err)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %q does not mention the limit", err)
	}
}

func TestExitErrorMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  ExitError
		want string
	}{
		{
			name: "without stderr",
			err:  ExitError{Host: "gluster-1", Program: "gluster", Code: 1},
			want: "gluster on gluster-1 exited with status 1",
		},
		{
			name: "with stderr",
			err:  ExitError{Host: "gluster-1", Program: "gluster", Code: 2, Stderr: []byte("volume not found\n")},
			want: "gluster on gluster-1 exited with status 2: volume not found",
		},
		{
			name: "stderr is trimmed",
			err:  ExitError{Host: "g", Program: "gluster", Code: 1, Stderr: []byte("  \t spaced \n\n ")},
			want: "gluster on g exited with status 1: spaced",
		},
		{
			name: "control characters become spaces",
			err:  ExitError{Host: "g", Program: "gluster", Code: 1, Stderr: []byte("a\x1b[31mb\r\nc\x00d")},
			want: "gluster on g exited with status 1: a [31mb  c d",
		},
		{
			name: "stderr of only white space is not shown",
			err:  ExitError{Host: "g", Program: "gluster", Code: 1, Stderr: []byte(" \n\t ")},
			want: "gluster on g exited with status 1",
		},
		{
			name: "long stderr is truncated",
			err:  ExitError{Host: "g", Program: "gluster", Code: 1, Stderr: []byte(strings.Repeat("x", maxExitStderrMessage+10))},
			want: "gluster on g exited with status 1: " + strings.Repeat("x", maxExitStderrMessage) + "...",
		},
		{
			name: "stderr of exactly the message limit is not marked as truncated",
			err:  ExitError{Host: "g", Program: "gluster", Code: 1, Stderr: []byte(strings.Repeat("y", maxExitStderrMessage))},
			want: "gluster on g exited with status 1: " + strings.Repeat("y", maxExitStderrMessage),
		},
		{
			name: "a multi-byte character split by the cut is dropped",
			err: ExitError{Host: "g", Program: "gluster", Code: 1,
				Stderr: []byte(strings.Repeat("z", maxExitStderrMessage-1) + "卷 tail")},
			want: "gluster on g exited with status 1: " + strings.Repeat("z", maxExitStderrMessage-1) + "...",
		},
		{
			name: "invalid UTF-8 in stderr is dropped",
			err:  ExitError{Host: "g", Program: "gluster", Code: 1, Stderr: []byte("bad \xff\xfe byte")},
			want: "gluster on g exited with status 1: bad  byte",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(tc.err.Error()) {
				t.Errorf("Error() = %q is not valid UTF-8", tc.err.Error())
			}
			if got := len(tc.err.Error()); got > 400 {
				t.Errorf("Error() is %d bytes long, want at most 400", got)
			}
		})
	}
}

func TestExitErrorIsNotAnErrConnect(t *testing.T) {
	t.Parallel()

	exitErr := &ExitError{Host: "gluster-1", Program: "gluster", Code: 1}
	var err error = exitErr
	if errors.Is(err, ErrConnect) {
		t.Fatal("an ExitError must not be an ErrConnect: the command did run")
	}
	var target *ExitError
	if !errors.As(err, &target) || target.Code != 1 {
		t.Fatalf("errors.As(*ExitError) = %v, want the ExitError", target)
	}
}

func TestLimitedWriter(t *testing.T) {
	t.Parallel()

	calls := 0
	writer := &limitedWriter{limit: 4, onExceed: func() { calls++ }}
	if n, err := writer.Write([]byte("abcd")); err != nil || n != 4 {
		t.Fatalf("Write(abcd) = %d, %v, want 4, nil", n, err)
	}
	if writer.Exceeded() || calls != 0 {
		t.Fatalf("Exceeded() = %v, onExceed calls = %d after writes within the limit, want false, 0", writer.Exceeded(), calls)
	}
	for i := 0; i < 2; i++ {
		if _, err := writer.Write([]byte("e")); !errors.Is(err, errOutputLimit) {
			t.Fatalf("Write above the limit = %v, want errOutputLimit", err)
		}
	}
	if !writer.Exceeded() {
		t.Error("Exceeded() = false after a write above the limit, want true")
	}
	if calls != 1 {
		t.Errorf("onExceed was called %d times, want exactly once", calls)
	}
	if got := string(writer.Bytes()); got != "abcd" {
		t.Errorf("Bytes() = %q, want %q", got, "abcd")
	}
}

func TestValidateCommandNamesTheRejectedProgramOnce(t *testing.T) {
	t.Parallel()

	err := validateCommand("gluster-1", []string{"sh", "-c", "id"})
	if err == nil {
		t.Fatal("validateCommand(sh) = nil, want an error")
	}
	if msg := err.Error(); !strings.Contains(msg, `program "sh" is not allowlisted`) {
		t.Errorf("error %q does not name the program exactly once quoted", msg)
	}
}

func TestBoundedWriterDropsTheExcess(t *testing.T) {
	t.Parallel()

	writer := &boundedWriter{limit: 5}
	if n, err := writer.Write([]byte("abc")); err != nil || n != 3 {
		t.Fatalf("Write(abc) = %d, %v, want 3, nil", n, err)
	}
	if n, err := writer.Write([]byte("defgh")); err != nil || n != 5 {
		t.Fatalf("Write(defgh) = %d, %v, want 5, nil", n, err)
	}
	if got := string(writer.Bytes()); got != "abcde" {
		t.Errorf("Bytes() = %q, want %q", got, "abcde")
	}
	if n, err := writer.Write([]byte("ij")); err != nil || n != 2 {
		t.Fatalf("Write after the limit = %d, %v, want 2, nil", n, err)
	}
	if got := string(writer.Bytes()); got != "abcde" {
		t.Errorf("Bytes() after the limit = %q, want %q", got, "abcde")
	}
}

func TestValidateArgumentAcceptsControlFreeStrings(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"", "gluster", "a b", "--mode=script", "卷", "it's", "$(id)"} {
		if err := validateArgument(arg); err != nil {
			t.Errorf("validateArgument(%q) = %v, want nil", arg, err)
		}
	}
}

func TestQuoteValueHidesLongValues(t *testing.T) {
	t.Parallel()

	if got := quoteValue("abc"); got != `"abc"` {
		t.Errorf("quoteValue(abc) = %s, want %q", got, `"abc"`)
	}
	long := strings.Repeat("z", 100)
	got := quoteValue(long)
	if strings.Contains(got, long) {
		t.Errorf("quoteValue(long) = %s, must not echo the value", got)
	}
	if want := fmt.Sprintf("value of length %d", len(long)); got != want {
		t.Errorf("quoteValue(long) = %s, want %s", got, want)
	}
}

func TestTruncateOutput(t *testing.T) {
	t.Parallel()

	if got := truncateOutput([]byte("abc"), 5); string(got) != "abc" {
		t.Errorf("truncateOutput below the limit = %q, want abc", got)
	}
	if got := truncateOutput([]byte("abcdef"), 3); string(got) != "abc" {
		t.Errorf("truncateOutput above the limit = %q, want abc", got)
	}
}

func TestExecutorContextCancelledBeforeTheCommandIsAnInterruption(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := interruptedError(ctx.Err())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interruptedError = %v, want errors.Is(err, context.Canceled)", err)
	}
	if errors.Is(err, ErrConnect) {
		t.Fatal("a cancellation must not wrap ErrConnect")
	}
}

func TestValidateHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		host    string
		wantErr string
	}{
		{name: "plain host name", host: "gluster-1"},
		{name: "host name at the length limit", host: strings.Repeat("a", maxHostLength)},
		{name: "host name above the length limit", host: strings.Repeat("a", maxHostLength+1), wantErr: "limit"},
		{name: "IPv6 literal is accepted", host: "fd00::1"},
		{name: "empty host", host: "", wantErr: "empty host"},
		{name: "host that is not valid UTF-8", host: "gluster-\xff", wantErr: "UTF-8"},
		{name: "host with a NUL byte", host: "gluster\x00", wantErr: "control"},
		{name: "host with a space", host: "gluster 1", wantErr: "white space"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateHost(tc.host)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateHost(%q) = %v, want nil", tc.host, err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidCommand) {
				t.Fatalf("validateHost(%q) = %v, want ErrInvalidCommand", tc.host, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validateHost(%q) = %q, want it to mention %q", tc.host, err, tc.wantErr)
			}
		})
	}
}

func TestAllHostsFailedError(t *testing.T) {
	t.Parallel()

	first := fmt.Errorf("%w: cannot connect to gluster-1", ErrConnect)
	second := fmt.Errorf("%w: cannot connect to gluster-2", ErrConnect)
	err := &AllHostsFailedError{Errors: []error{first, second}}

	want := "no glusterfs server is reachable: " + first.Error() + "; " + second.Error()
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrConnect) {
		t.Error("errors.Is(err, ErrConnect) = false, want true")
	}
	if got := len(err.Unwrap()); got != 2 {
		t.Errorf("Unwrap() has %d errors, want 2", got)
	}
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Error("errors.Is does not find the attempt errors")
	}
	empty := &AllHostsFailedError{}
	if got := empty.Error(); got != "no glusterfs server is reachable: " {
		t.Errorf("Error() of an empty error = %q", got)
	}
}
