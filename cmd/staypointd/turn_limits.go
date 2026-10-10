package main

import (
	"log/slog"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// turnLimits holds the per-turn time limits from config.toml in
// orchestrator.RunConfig form. Zero values mean no turn_timeout and the
// default 20m stall_timeout, which is also what tests get.
var turnLimits struct {
	turn  time.Duration
	stall time.Duration
}

// opsTokens holds each live run's ops token in memory
// (orchestrator.RunConfig.OpsTokens); nil in tests, so runs get none.
var opsTokens *opstools.RunTokens

// setTurnLimits loads turn_timeout and stall_timeout. There is no fixed cap
// on how long a run or an active turn may take; only a turn with no agent
// activity for stall_timeout is stopped.
func setTurnLimits(cfg *config.Config) {
	turn, err := cfg.TurnTimeoutOrDefault()
	if err != nil {
		slog.Warn("config: bad turn_timeout; using no limit", slog.Any("error", err))
	}
	stall, err := cfg.StallTimeoutOrDefault()
	if err != nil {
		slog.Warn("config: bad stall_timeout; using default", slog.Any("error", err))
	}
	turnLimits.turn = turn
	turnLimits.stall = stall
	if stall == 0 {
		turnLimits.stall = -1 // RunConfig: negative = stall check off
	}
	slog.Info("turn limits", slog.Duration("turn_timeout", turn), slog.Duration("stall_timeout", stall))
}
