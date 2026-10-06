package orchestrator

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"time"
)

// TailTranscript reads transcript.jsonl as it is written by agy and feeds events
// into the StepRecorder to populate the StayPoint timeline.
func TailTranscript(ctx context.Context, transcriptPath string, root string, feed func(StepDelta)) {
	var file *os.File
	var err error

	// Wait for the file to be created.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		file, err = os.Open(transcriptPath)
		if err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return
		}

		var ev struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				Name string          `json:"name"`
				Args json.RawMessage `json:"args"`
			} `json:"tool_calls"`
			Content string `json:"content"`
			Status  string `json:"status"`
			Source  string `json:"source"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}

		if ev.Type == "PLANNER_RESPONSE" {
			if ev.Thinking != "" {
				feed(StepDelta{
					Kind: StepDeltaThinking,
					Text: ev.Thinking,
				})
			}
			if ev.Content != "" {
				feed(StepDelta{
					Kind: StepDeltaText,
					Text: ev.Content,
				})
			}
			for _, tc := range ev.ToolCalls {
				feed(StepDelta{
					Kind:      StepDeltaToolUse,
					ToolName:  tc.Name,
					ToolInput: string(tc.Args),
				})
				// For the UI, we just extract title using extractToolTitle
			}
		}
	}
}
