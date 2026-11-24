package procs

import "testing"

func TestParseStat(t *testing.T) {
	t.Parallel()

	// Fields after the command: state ppid pgrp session tty tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice threads itrealvalue starttime
	tail := " S 1 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10"

	cases := []struct {
		name      string
		line      string
		wantToken uint64
		wantState byte
		wantErr   bool
	}{
		{"simple command", "42 (sleep)" + tail, 987654, 'S', false},
		{"command with spaces and parens", "42 (my (weird) cmd)" + tail, 987654, 'S', false},
		{"zombie", "42 (z)" + " Z" + tail[2:], 987654, 'Z', false},
		{"missing paren", "42 sleep" + tail, 0, 0, true},
		{"truncated", "42 (sleep) S 1 1", 0, 0, true},
		{"non-numeric starttime", "42 (x) S 1 1 1 0 -1 4 1 0 0 0 1 2 0 0 20 0 1 0 abc", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, state, err := parseStat(tc.line)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got token=%d state=%q", token, state)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStat: %v", err)
			}
			if token != tc.wantToken || state != tc.wantState {
				t.Fatalf("got (%d, %q), want (%d, %q)", token, state, tc.wantToken, tc.wantState)
			}
		})
	}
}
