package router

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. DefaultKindChains — built-in routing table shape
// ---------------------------------------------------------------------------

func TestDefaultKindChains_AllFourKindsPresent(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Fatal("DefaultKindChains() returned nil; expected map with 4 kind entries")
	}
	for _, k := range []WorkKind{WorkKindCoding, WorkKindArchitecture, WorkKindPlanning, WorkKindQA} {
		if _, ok := chains[k]; !ok {
			t.Errorf("DefaultKindChains missing kind %q", k)
		}
	}
}

func TestDefaultKindChains_CodingChain(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Fatal("DefaultKindChains() returned nil")
	}
	coding := chains[WorkKindCoding]
	if len(coding) == 0 {
		t.Fatal("coding chain is empty")
	}

	// Primary must be Claude Opus
	if !strings.Contains(coding[0].Provider, "claude") || !strings.Contains(coding[0].Model, "opus") {
		t.Errorf("coding[0]: want claude-opus primary, got provider=%q model=%q",
			coding[0].Provider, coding[0].Model)
	}
	// Board rule (GeminiCodeForbidden): code is Claude only, no Gemini backup.
	for _, k := range []WorkKind{WorkKindCoding, WorkKindQA} {
		for _, slot := range chains[k] {
			if strings.Contains(slot.Provider, "gemini") {
				t.Errorf("%s chain must never contain Gemini, got %+v", k, chains[k])
			}
		}
	}
}

func TestDefaultKindChains_ArchitectureChain(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Fatal("DefaultKindChains() returned nil")
	}
	arch := chains[WorkKindArchitecture]
	if len(arch) == 0 {
		t.Fatal("architecture chain is empty")
	}

	// Primary: Gemini 3.1 Pro
	if !strings.Contains(arch[0].Provider, "gemini") || !strings.Contains(arch[0].Model, "3.1-pro") {
		t.Errorf("architecture[0]: want gemini-3.1-pro primary, got provider=%q model=%q",
			arch[0].Provider, arch[0].Model)
	}
	// Must have a cloud slot (disabled by default) and a claude-opus fallback
	hasCloud := false
	hasOpusFallback := false
	for _, s := range arch[1:] {
		if strings.Contains(s.Provider, "cloud") {
			hasCloud = true
		}
		if strings.Contains(s.Provider, "claude") && strings.Contains(s.Model, "opus") {
			hasOpusFallback = true
		}
	}
	if !hasCloud {
		t.Error("architecture chain must include a cloud slot")
	}
	if !hasOpusFallback {
		t.Error("architecture chain must include a claude-opus fallback slot")
	}
}

func TestDefaultKindChains_CloudSlotDisabledByDefault(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Fatal("DefaultKindChains() returned nil")
	}
	// Every cloud slot must start disabled until STA-410 lands.
	for kind, chain := range chains {
		for i, s := range chain {
			if strings.Contains(s.Provider, "cloud") && s.Enabled {
				t.Errorf("kind %q slot[%d] cloud slot must be disabled by default", kind, i)
			}
		}
	}
}

func TestDefaultKindChains_PlanningChain(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Fatal("DefaultKindChains() returned nil")
	}
	planning := chains[WorkKindPlanning]
	if len(planning) == 0 {
		t.Fatal("planning chain is empty")
	}
	// Primary: Gemini 3.8 Flash
	if !strings.Contains(planning[0].Provider, "gemini") || !strings.Contains(planning[0].Model, "3.8-flash") {
		t.Errorf("planning[0]: want gemini-3.8-flash primary, got provider=%q model=%q",
			planning[0].Provider, planning[0].Model)
	}
	// Last non-cloud slot: claude-sonnet
	var lastReal KindSlot
	for _, s := range planning {
		if !strings.Contains(s.Provider, "cloud") {
			lastReal = s
		}
	}
	if !strings.Contains(lastReal.Provider, "claude") || !strings.Contains(lastReal.Model, "sonnet") {
		t.Errorf("planning chain last real slot: want claude-sonnet, got provider=%q model=%q",
			lastReal.Provider, lastReal.Model)
	}
}

func TestDefaultKindChains_QAChain(t *testing.T) {
	qa := DefaultKindChains()[WorkKindQA]
	if len(qa) != 1 || qa[0].Provider != "claude-opus" || qa[0].Model != "opus" {
		t.Fatalf("qa (writes tests) must be Claude Opus only, got %+v", qa)
	}
}

func TestDefaultKindChains_NonCodeKindsGeminiFirst(t *testing.T) {
	chains := DefaultKindChains()
	for _, k := range []WorkKind{WorkKindReview, WorkKindArchitecture, WorkKindPlanning, WorkKindDocs} {
		c := chains[k]
		if len(c) < 2 || !strings.HasPrefix(c[0].Provider, "gemini") || !strings.HasPrefix(c[1].Provider, "claude-") {
			t.Errorf("%s: want Gemini first then Claude, got %+v", k, c)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. ResolveKindChain — chain resolution with pacer state
//    Tests define inline chains so they fail (not skip) even when
//    DefaultKindChains is not yet implemented.
// ---------------------------------------------------------------------------

// testCodingChain is the expected coding chain used by resolution tests.
var testCodingChain = []KindSlot{
	{Provider: "claude-opus", Model: "opus", PoolID: PoolPersonalClaude, Enabled: true},
	{Provider: "gemini-3.1-pro", Model: "gemini-3.1-pro-high", PoolID: PoolGeminiNative, Enabled: true},
}

// testArchChain is the expected architecture chain used by resolution tests.
var testArchChain = []KindSlot{
	{Provider: "gemini-3.1-pro", Model: "gemini-3.1-pro-high", PoolID: PoolGeminiNative, Enabled: true},
	{Provider: "claude-cloud", Model: "opus", PoolID: "", Enabled: false}, // off until STA-410
	{Provider: "claude-opus", Model: "opus", PoolID: PoolPersonalClaude, Enabled: true},
}

func TestResolveKindChain_CodingPrimaryAvailable(t *testing.T) {
	chains := map[WorkKind][]KindSlot{WorkKindCoding: testCodingChain}
	pacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolPersonalClaude: {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
			PoolWorkClaude:     {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
			PoolGeminiNative:   {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
		},
	}

	slot := ResolveKindChain(WorkKindCoding, chains, pacer)
	if slot == nil {
		t.Fatal("ResolveKindChain returned nil; expected claude-opus slot")
	}
	if !strings.Contains(slot.Provider, "claude") || !strings.Contains(slot.Model, "opus") {
		t.Errorf("expected claude-opus primary, got provider=%q model=%q", slot.Provider, slot.Model)
	}
}

func TestResolveKindChain_CodingFallsBackWhenClaudeLocked(t *testing.T) {
	chains := map[WorkKind][]KindSlot{WorkKindCoding: testCodingChain}
	pacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolPersonalClaude: {IsLocked: true},
			PoolWorkClaude:     {IsLocked: true},
			PoolGeminiNative:   {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
		},
	}

	slot := ResolveKindChain(WorkKindCoding, chains, pacer)
	if slot == nil {
		t.Fatal("ResolveKindChain returned nil; expected gemini-3.1-pro fallback")
	}
	if !strings.Contains(slot.Provider, "gemini") || !strings.Contains(slot.Model, "3.1-pro") {
		t.Errorf("expected gemini-3.1-pro fallback with claude locked, got provider=%q model=%q",
			slot.Provider, slot.Model)
	}
}

func TestResolveKindChain_CloudSlotSkippedWhenDisabled(t *testing.T) {
	chains := map[WorkKind][]KindSlot{WorkKindArchitecture: testArchChain}
	// Gemini primary locked, cloud slot disabled → should reach claude-opus
	pacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolGeminiNative:   {IsLocked: true},
			PoolPersonalClaude: {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
			PoolWorkClaude:     {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
		},
	}

	slot := ResolveKindChain(WorkKindArchitecture, chains, pacer)
	if slot == nil {
		t.Fatal("ResolveKindChain returned nil; expected claude-opus fallback")
	}
	if strings.Contains(slot.Provider, "cloud") {
		t.Errorf("disabled cloud slot must be skipped, but got provider=%q", slot.Provider)
	}
	if !strings.Contains(slot.Provider, "claude") || !strings.Contains(slot.Model, "opus") {
		t.Errorf("expected claude-opus after skipping disabled cloud slot, got provider=%q model=%q",
			slot.Provider, slot.Model)
	}
}

func TestResolveKindChain_CloudSlotSkippedWhenExpired(t *testing.T) {
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	archWithExpiredCloud := []KindSlot{
		{Provider: "gemini-3.1-pro", Model: "gemini-3.1-pro-high", PoolID: PoolGeminiNative, Enabled: true},
		{Provider: "claude-cloud", Model: "opus", PoolID: "", Enabled: true, CloudCreditExpires: past},
		{Provider: "claude-opus", Model: "opus", PoolID: PoolPersonalClaude, Enabled: true},
	}
	chains := map[WorkKind][]KindSlot{WorkKindArchitecture: archWithExpiredCloud}

	pacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolGeminiNative:   {IsLocked: true},
			PoolPersonalClaude: {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
			PoolWorkClaude:     {IsLocked: false, FiveHour: QuotaWindow{RemainingPct: 80}},
		},
	}

	slot := ResolveKindChain(WorkKindArchitecture, chains, pacer)
	if slot == nil {
		t.Fatal("expected a slot (claude-opus) after skipping expired cloud slot")
	}
	if strings.Contains(slot.Provider, "cloud") {
		t.Errorf("expired cloud slot must be skipped, got provider=%q", slot.Provider)
	}
}

func TestResolveKindChain_ReturnsNilWhenAllLocked(t *testing.T) {
	chains := map[WorkKind][]KindSlot{WorkKindCoding: testCodingChain}
	pacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolPersonalClaude: {IsLocked: true},
			PoolWorkClaude:     {IsLocked: true},
			PoolGeminiNative:   {IsLocked: true},
		},
	}

	slot := ResolveKindChain(WorkKindCoding, chains, pacer)
	if slot != nil {
		t.Errorf("expected nil when all pools locked, got provider=%q", slot.Provider)
	}
}

// ---------------------------------------------------------------------------
// 3. Model passthrough — KindSlot.Model is the explicit model flag
// ---------------------------------------------------------------------------

// allTestChains covers all four kinds for model-shape tests.
var allTestChains = map[WorkKind][]KindSlot{
	WorkKindCoding: {
		{Provider: "claude-opus", Model: "opus", PoolID: PoolPersonalClaude, Enabled: true},
		{Provider: "gemini-3.1-pro", Model: "gemini-3.1-pro-high", PoolID: PoolGeminiNative, Enabled: true},
	},
	WorkKindArchitecture: {
		{Provider: "gemini-3.1-pro", Model: "gemini-3.1-pro-high", PoolID: PoolGeminiNative, Enabled: true},
		{Provider: "claude-cloud", Model: "opus", PoolID: "", Enabled: false},
		{Provider: "claude-opus", Model: "opus", PoolID: PoolPersonalClaude, Enabled: true},
	},
	WorkKindPlanning: {
		{Provider: "gemini-3.8-flash", Model: "gemini-3.8-flash-high", PoolID: PoolGeminiNative, Enabled: true},
		{Provider: "claude-cloud", Model: "opus", PoolID: "", Enabled: false},
		{Provider: "claude-sonnet", Model: "sonnet", PoolID: PoolPersonalClaude, Enabled: true},
	},
	WorkKindQA: {
		{Provider: "gemini-3.8-flash", Model: "gemini-3.8-flash-high", PoolID: PoolGeminiNative, Enabled: true},
		{Provider: "claude-sonnet", Model: "sonnet", PoolID: PoolPersonalClaude, Enabled: true},
	},
}

func TestKindSlot_ModelIsExplicit(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		// Fall back to expected chains to test the shape contract.
		chains = allTestChains
		t.Log("DefaultKindChains not yet implemented; using expected test chains")
	}

	for kind, chain := range chains {
		for i, s := range chain {
			if s.Enabled && s.PoolID != "" {
				if s.Model == "" {
					t.Errorf("kind %q slot[%d] (provider=%q): Model must be non-empty (CLI flag must be explicit)",
						kind, i, s.Provider)
				}
			}
		}
	}
	// Specifically assert DefaultKindChains returns the expected chain shapes.
	got := DefaultKindChains()
	if got == nil {
		t.Error("DefaultKindChains() returned nil; expected map with chains for all 4 work kinds")
	}
}

func TestKindSlot_ClaudeModelIsOpusOrSonnet(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Error("DefaultKindChains() returned nil; cannot verify claude model names")
		return
	}

	for kind, chain := range chains {
		for i, s := range chain {
			if !strings.Contains(s.Provider, "claude") || strings.Contains(s.Provider, "cloud") {
				continue
			}
			if s.Model != "opus" && s.Model != "sonnet" {
				t.Errorf("kind %q slot[%d] claude provider: model must be 'opus' or 'sonnet', got %q",
					kind, i, s.Model)
			}
		}
	}
}

func TestKindSlot_GeminiModelHasSuffixHigh(t *testing.T) {
	chains := DefaultKindChains()
	if chains == nil {
		t.Error("DefaultKindChains() returned nil; cannot verify gemini model names")
		return
	}

	for kind, chain := range chains {
		for i, s := range chain {
			if !strings.Contains(s.Provider, "gemini") {
				continue
			}
			if !strings.HasSuffix(s.Model, "-high") {
				t.Errorf("kind %q slot[%d] gemini provider: model must end with '-high', got %q",
					kind, i, s.Model)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 4. Bidirectional tier map
// ---------------------------------------------------------------------------

func TestPairModelBidirectional_ClaudeOpusToGemini(t *testing.T) {
	paired := PairModelBidirectional("opus")
	if !strings.Contains(paired, "3.1-pro") {
		t.Errorf("PairModelBidirectional(opus): want gemini-3.1-pro, got %q", paired)
	}
}

func TestPairModelBidirectional_GeminiProToClaude(t *testing.T) {
	for _, input := range []string{"gemini-3.1-pro", "gemini-3.1-pro-high"} {
		paired := PairModelBidirectional(input)
		if !strings.Contains(paired, "opus") {
			t.Errorf("PairModelBidirectional(%q): want claude-opus, got %q", input, paired)
		}
	}
}

func TestPairModelBidirectional_ClaudeSonnetToGeminiFlash(t *testing.T) {
	paired := PairModelBidirectional("sonnet")
	if !strings.Contains(paired, "3.8-flash") {
		t.Errorf("PairModelBidirectional(sonnet): want gemini-3.8-flash, got %q", paired)
	}
}

func TestPairModelBidirectional_GeminiFlashToSonnet(t *testing.T) {
	for _, input := range []string{"gemini-3.8-flash", "gemini-3.8-flash-high"} {
		paired := PairModelBidirectional(input)
		if !strings.Contains(paired, "sonnet") {
			t.Errorf("PairModelBidirectional(%q): want claude-sonnet, got %q", input, paired)
		}
	}
}

func TestPairModelBidirectional_UnknownReturnsEmpty(t *testing.T) {
	if got := PairModelBidirectional("gpt-4"); got != "" {
		t.Errorf("PairModelBidirectional(gpt-4): want empty string, got %q", got)
	}
}

func TestPairModelBidirectional_IsSymmetric(t *testing.T) {
	pairs := [][2]string{
		{"opus", "gemini-3.1-pro"},
		{"sonnet", "gemini-3.8-flash"},
	}
	for _, p := range pairs {
		ab := PairModelBidirectional(p[0])
		if !strings.Contains(ab, p[1]) {
			t.Errorf("PairModelBidirectional(%q) → %q, want to contain %q", p[0], ab, p[1])
		}
		ba := PairModelBidirectional(p[1])
		if !strings.Contains(ba, p[0]) {
			t.Errorf("PairModelBidirectional(%q) → %q, want to contain %q (symmetry check)", p[1], ba, p[0])
		}
	}
}

// ---------------------------------------------------------------------------
// 5. FallbackPairingMatrix is one-directional today → verify bidirectional gap
// ---------------------------------------------------------------------------

func TestFallbackPairingMatrix_OnlyClaudeSide(t *testing.T) {
	// This test documents the current state: FallbackPairingMatrix only maps
	// Claude → Gemini, not Gemini → Claude.  PairModelBidirectional must cover
	// both directions for STA-316.
	geminiModel, _, _ := FallbackPairingMatrix("opus", "high")
	if !strings.Contains(geminiModel, "gemini") {
		t.Errorf("FallbackPairingMatrix(opus): expected gemini model, got %q", geminiModel)
	}
	// Passing a gemini model should not return a claude model (not implemented yet).
	// This confirms the gap that PairModelBidirectional fills.
	result, _, _ := FallbackPairingMatrix("gemini-3.1-pro", "high")
	if strings.Contains(strings.ToLower(result), "claude") {
		t.Logf("FallbackPairingMatrix already handles gemini→claude: %q (unexpected)", result)
	}
}
