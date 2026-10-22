//go:build quark_live

package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The child has no networking code. It exists solely to exercise the actual
// pipe, output budget, process exit and deadline boundary using synthetic data.
func TestQuarkSpeedNativeProcessFixture(t *testing.T) {
	mode := os.Getenv("OMC_QUARK_SPEED_SYNTHETIC_CHILD")
	if mode == "" {
		return
	}
	inputBytes, _ := io.ReadAll(io.LimitReader(os.Stdin, 16<<10))
	var input quarkNativeSpeedInput
	if json.Unmarshal(inputBytes, &input) != nil || input.URL != "https://cdn.invalid/original?token=synthetic-private" || input.Authorization != "" {
		os.Exit(3)
	}
	fmt.Fprintln(os.Stderr, "synthetic-private must never be relayed")
	output := quarkNativeSpeedOutput{Version: 1, Mode: input.Mode, Status: 206, Bytes: input.MaxBytes, ElapsedMS: 2, RangeValid: true, SHA256: strings.Repeat("a", 64), Completed: true, SampleComplete: true, WholeRangeConsumed: input.RangeMode == "closed", RangeSpanBytes: input.MaxBytes, ReadCapBytes: input.MaxBytes}
	if input.RangeMode == "open" {
		output.RangeSpanBytes = input.ExpectedSize - input.RangeStart
	}
	exitCode := 0
	switch mode {
	case "oversized":
		fmt.Print(strings.Repeat("x", 17<<10))
		os.Exit(1)
	case "secret-error":
		output.Error = "https://cdn.invalid?token=synthetic-private"
	case "secret-hash":
		output.SHA256 = "synthetic-private"
	case "partial":
		output.Bytes, output.Completed, output.SampleComplete, output.WholeRangeConsumed, output.TimedOut, output.Error = 5, false, false, false, true, "timeout"
		exitCode = 1
	case "bad-exit":
		exitCode = 7
	case "hang":
		time.Sleep(10 * time.Second)
	case "false-size":
		output.Bytes++
	}
	_ = json.NewEncoder(os.Stdout).Encode(output)
	if mode == "trailing-output" {
		fmt.Print("synthetic-private")
	}
	os.Exit(exitCode)
}

func TestQuarkSpeedNativeProcessContract(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("test executable unavailable")
	}
	for _, tc := range []struct {
		mode, expectedError string
	}{
		{"complete", ""},
		{"partial", ""},
		{"oversized", "native_speed_output_invalid"},
		{"secret-error", "native_speed_output_invalid"},
		{"secret-hash", "native_speed_output_invalid"},
		{"bad-exit", "native_speed_child_failed"},
		{"false-size", "native_speed_output_invalid"},
		{"trailing-output", "native_speed_output_invalid"},
		{"hang", "native_speed_child_timeout"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			input := quarkNativeInput("native-bridge-open-head", "https://cdn.invalid/original?token=synthetic-private", 0, 64, 1000, time.Second)
			ctx := context.Background()
			if tc.mode == "hang" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			result, err := quarkRunNativeSpeed(ctx, input, func(childContext context.Context) *exec.Cmd {
				cmd := exec.CommandContext(childContext, executable, "-test.run=^TestQuarkSpeedNativeProcessFixture$")
				cmd.Env = append(os.Environ(), "OMC_QUARK_SPEED_SYNTHETIC_CHILD="+tc.mode)
				return cmd
			})
			if tc.expectedError != "" {
				if err == nil || err.Error() != tc.expectedError {
					t.Fatal("unexpected safe child error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result)
			if strings.Contains(string(encoded), "synthetic-private") || tc.mode == "partial" && (result.SampleComplete || result.SHA256 != "" || result.Bytes != 5) || tc.mode == "complete" && (!result.SampleComplete || result.WholeRangeConsumed || result.RangeSpanBytes != 1000) {
				t.Fatal("unsafe or inaccurate native output")
			}
		})
	}
}
