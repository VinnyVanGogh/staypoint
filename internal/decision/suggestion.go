package decision

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

// GenerateInteractionSuggestion asks the local advisory model to answer an interaction (like ask_user_questions).
// It runs in the background and updates the interaction payload with "ai_suggestion".
func GenerateInteractionSuggestion(db *sql.DB, interactionID int, taskID string, kind string, payloadJSON string, taskCtx string) {
	if kind != "ask_user_questions" && kind != "request_confirmation" {
		return // Only suggest on questions or confirmations
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		client, err := New() // defaults to local tev1
		if err != nil {
			slog.Warn("decision: could not init advisory client for suggestion", "err", err)
			return
		}

		// Parse the interaction payload to extract the question
		var pData map[string]any
		if err := json.Unmarshal([]byte(payloadJSON), &pData); err != nil {
			return
		}

		questionStr := ""
		if q, ok := pData["questions"].([]any); ok && len(q) > 0 {
			if qMap, ok := q[0].(map[string]any); ok {
				if qText, ok := qMap["question"].(string); ok {
					questionStr = qText
				}
			}
		} else if q, ok := pData["message"].(string); ok {
			questionStr = q
		}
		
		if questionStr == "" {
			return
		}

		req := AnswerRequest{
			Context:  taskCtx,
			Question: questionStr,
		}

		answer, err := client.Answer(ctx, req)
		if err != nil {
			slog.Warn("decision: suggestion generation failed", "task_id", taskID, "err", err)
			return
		}

		// Update payload with ai_suggestion
		pData["ai_suggestion"] = answer
		newPayload, _ := json.Marshal(pData)

		_, err = db.Exec("UPDATE task_interactions SET payload = ? WHERE id = ?", string(newPayload), interactionID)
		if err != nil {
			slog.Warn("decision: failed to save suggestion to DB", "err", err)
			return
		}

		slog.Info("decision: suggestion saved", "interaction_id", interactionID, "task_id", taskID)
	}()
}
