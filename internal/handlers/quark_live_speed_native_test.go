//go:build quark_live

package handlers_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

type quarkNativeSpeedInput struct {
	Version        int    `json:"version"`
	Mode           string `json:"mode"`
	URL            string `json:"url"`
	MaxBytes       int64  `json:"max_bytes"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
	RangeStart     int64  `json:"range_start"`
	RangeMode      string `json:"range_mode"`
	ExpectedSize   int64  `json:"expected_size"`
	Authorization  string `json:"authorization"`
}

type quarkNativeSpeedOutput struct {
	Version                 int     `json:"version"`
	Mode                    string  `json:"mode"`
	Status                  int     `json:"status"`
	Bytes                   int64   `json:"bytes"`
	ElapsedMS               int64   `json:"elapsed_ms"`
	TTFBMS                  *uint64 `json:"ttfb_ms"`
	BytesPerSecond          uint64  `json:"bytes_per_second"`
	RangeValid              bool    `json:"range_valid"`
	SHA256                  string  `json:"sha256"`
	Completed               bool    `json:"completed"`
	SampleComplete          bool    `json:"sample_complete"`
	WholeRangeConsumed      bool    `json:"whole_range_consumed"`
	RangeSpanBytes          int64   `json:"range_span_bytes"`
	ReadCapBytes            int64   `json:"read_cap_bytes"`
	TimedOut                bool    `json:"timed_out"`
	Error                   string  `json:"error"`
	LocalBridgePort         uint16  `json:"local_bridge_port"`
	ProxyEnvironmentPresent bool    `json:"proxy_environment_present"`
}

type quarkNativeBoundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *quarkNativeBoundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (16 << 10) - b.Len()
	if len(p) > remaining {
		p, b.overflow = p[:remaining], true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// Only this typed, validated output may be logged. stderr and raw stdout are
// never surfaced even on child failure: URLs can occur in transport errors.
func quarkRunNativeSpeed(ctx context.Context, input quarkNativeSpeedInput, start func(context.Context) *exec.Cmd) (quarkNativeSpeedOutput, error) {
	var output quarkNativeSpeedOutput
	payload, err := json.Marshal(input)
	if err != nil || len(payload) > 16<<10 || input.TimeoutSeconds < 1 || input.TimeoutSeconds > 20 || input.MaxBytes < 1 || input.MaxBytes > 32<<20 || input.RangeStart < 0 || input.RangeStart > input.ExpectedSize-input.MaxBytes || input.Authorization != "" {
		return output, errors.New("native_speed_input_invalid")
	}
	childContext, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds+5)*time.Second)
	defer cancel()
	command := start(childContext)
	command.Stdin = bytes.NewReader(payload)
	var captured quarkNativeBoundedOutput
	command.Stdout, command.Stderr = &captured, io.Discard
	command.WaitDelay = 2 * time.Second
	runErr := command.Run()
	if childContext.Err() != nil {
		return output, errors.New("native_speed_child_timeout")
	}
	if captured.overflow {
		return output, errors.New("native_speed_output_invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(captured.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&output) != nil || decoder.Decode(new(any)) != io.EOF {
		return quarkNativeSpeedOutput{}, errors.New("native_speed_output_invalid")
	}
	allowedErrors := map[string]bool{"": true, "bridge_start": true, "client_start": true, "timeout": true, "connect": true, "redirect": true, "body": true, "request": true, "range_rejected": true, "length_rejected": true, "encoding_rejected": true, "overlong_body": true, "short_body": true}
	if output.Version != 1 || output.Mode != input.Mode || !allowedErrors[output.Error] || output.Status < 0 || output.Status > 599 || output.Bytes < 0 || output.Bytes > input.MaxBytes || output.ElapsedMS < 0 || output.ElapsedMS > (input.TimeoutSeconds+5)*1000 || output.Completed != output.SampleComplete || output.ReadCapBytes != input.MaxBytes || output.RangeSpanBytes < 0 || output.RangeSpanBytes > input.ExpectedSize-input.RangeStart {
		return quarkNativeSpeedOutput{}, errors.New("native_speed_output_invalid")
	}
	if output.SHA256 != "" {
		digest, decodeErr := hex.DecodeString(output.SHA256)
		if decodeErr != nil || len(digest) != 32 {
			return quarkNativeSpeedOutput{}, errors.New("native_speed_output_invalid")
		}
	}
	expectedSpan := input.MaxBytes
	if input.RangeMode == "open" {
		expectedSpan = input.ExpectedSize - input.RangeStart
	}
	if output.RangeValid && (output.Status != 206 || output.RangeSpanBytes != expectedSpan) || output.Completed && (output.Bytes != input.MaxBytes || !output.RangeValid || output.TimedOut || output.Error != "" || output.SHA256 == "") || output.WholeRangeConsumed && (!output.SampleComplete || output.Bytes != output.RangeSpanBytes) {
		return quarkNativeSpeedOutput{}, errors.New("native_speed_output_invalid")
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 || output.Completed {
			return quarkNativeSpeedOutput{}, errors.New("native_speed_child_failed")
		}
	} else if !output.Completed {
		return quarkNativeSpeedOutput{}, errors.New("native_speed_child_failed")
	}
	// Partial byte hashes are deliberately omitted here to match the Go probe's
	// complete-sample hash contract, even if the child returns a partial hash.
	if !output.SampleComplete {
		output.SHA256 = ""
	}
	return output, nil
}

func quarkNativeVariant(variant string) bool { return strings.HasPrefix(variant, "native-") }

func quarkNativeInput(variant, location string, start, count, size int64, timeout time.Duration) quarkNativeSpeedInput {
	input := quarkNativeSpeedInput{Version: 1, Mode: "reqwest-direct", URL: location, MaxBytes: count, TimeoutSeconds: int64(timeout / time.Second), RangeStart: start, RangeMode: "closed", ExpectedSize: size}
	if strings.HasPrefix(variant, "native-bridge-") {
		input.Mode = "production-bridge"
	}
	if strings.Contains(variant, "-open-") {
		input.RangeMode = "open"
	}
	return input
}
