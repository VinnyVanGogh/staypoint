package orchestrator

// buildRawArgs is buildTurnArgs for a run that starts at turn 0 (tests only).
func buildRawArgs(taskID string, turn int, cfg RunConfig, brief taskBrief, newComments []harnessComment) []string {
	return buildTurnArgs(taskID, turn, turn == 0, "", cfg, brief, newComments)
}
