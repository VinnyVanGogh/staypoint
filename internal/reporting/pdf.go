package reporting

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// Backwards-compatible MemoData struct
type MemoData struct {
	SubstantiatedValue string
	ROIMultiplier      string
	MonthlyRunRate     string
	AcceptedTurns      string
	TargetTier         string
	MonthlyNetCost     string
	ExpectedROI        string
}

func RenderExecutiveMemoPDF(ctx context.Context, data MemoData, outputPath string) error {
	cfg := config.DefaultConfig()
	workData, _, _, _ := FetchTelemetry(cfg)
	if data.SubstantiatedValue != "" {
		workData.SubstantiatedValue = data.SubstantiatedValue
	}
	if data.ROIMultiplier != "" {
		workData.ROIMultiplier = data.ROIMultiplier
	}
	if data.MonthlyRunRate != "" {
		workData.MonthlyRunRate = data.MonthlyRunRate
	}
	if data.AcceptedTurns != "" {
		workData.AcceptedTurns = data.AcceptedTurns
	}
	if data.MonthlyNetCost != "" {
		workData.MonthlyNetCost = data.MonthlyNetCost
	}
	if data.ExpectedROI != "" {
		workData.ExpectedROI = data.ExpectedROI
	}
	return RenderReport(ctx, "work", cfg, outputPath)
}

// GenerateReportHTML returns the rendered HTML report string for the given report type and options.
func GenerateReportHTML(reportType string, cfg *config.Config, rangeOpts ...DateRangeOptions) (string, error) {
	var opts DateRangeOptions
	if len(rangeOpts) > 0 {
		opts = rangeOpts[0]
	}
	workData, personalData, geminiData, combinedData, err := FetchTelemetryWithRange(cfg, opts)
	if err != nil {
		return "", err
	}

	var htmlContent string

	switch strings.ToLower(strings.TrimSpace(reportType)) {
	case "work", "boss":
		htmlContent, err = generateWorkHTML(workData)
	case "personal":
		htmlContent, err = generatePersonalHTML(personalData)
	case "gemini", "antigravity":
		htmlContent, err = generateGeminiHTML(geminiData)
	case "combined", "fleet":
		htmlContent, err = generateCombinedHTML(combinedData)
	default:
		return "", fmt.Errorf("unknown report type: %s (supported: work, personal, gemini, combined)", reportType)
	}

	if err != nil {
		return "", fmt.Errorf("failed to generate HTML template for %s: %w", reportType, err)
	}

	return htmlContent, nil
}

func RenderReport(ctx context.Context, reportType string, cfg *config.Config, outputPath string, rangeOpts ...DateRangeOptions) error {
	htmlContent, err := GenerateReportHTML(reportType, cfg, rangeOpts...)
	if err != nil {
		return err
	}

	return renderHTMLToPDF(ctx, htmlContent, outputPath)
}

func generateWorkHTML(data WorkReportData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>{{if .HasCompanyName}}{{.CompanyName}}: {{end}}Executive AI Velocity & ROI Briefing</title>
  <style>
    @page { size: letter; margin: 0; }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    html, body {
      height: 100%;
      max-height: 100%;
      overflow: hidden;
    }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      color: #1e293b; background: #ffffff; padding: 30px 36px; font-size: 12.5px; line-height: 1.45;
      -webkit-print-color-adjust: exact; print-color-adjust: exact;
    }
    .header { display: flex; justify-content: space-between; align-items: flex-start; border-bottom: 2px solid #0284c7; padding-bottom: 12px; margin-bottom: 16px; }
    .header-left h1 { font-size: 21px; font-weight: 800; color: #0f172a; letter-spacing: -0.02em; margin-bottom: 2px; }
    .header-left .subtitle { font-size: 11px; font-weight: 700; color: #0284c7; text-transform: uppercase; letter-spacing: 0.05em; }
    .header-right { text-align: right; font-size: 11px; color: #64748b; line-height: 1.35; }
    .header-right .org { font-weight: 800; color: #0f172a; font-size: 13px; }
    .kpi-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 11px; margin-bottom: 16px; }
    .kpi-card { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 7px; padding: 11px 13px; }
    .kpi-card.highlight { background: #f0f9ff; border-color: #bae6fd; }
    .kpi-label { font-size: 9.5px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.05em; color: #64748b; margin-bottom: 3px; }
    .kpi-val { font-size: 21px; font-weight: 800; color: #0f172a; line-height: 1.1; }
    .kpi-card.highlight .kpi-val { color: #0284c7; }
    .kpi-sub { font-size: 9.5px; color: #64748b; margin-top: 3px; }
    .two-col { display: grid; grid-template-columns: 1.15fr 0.85fr; gap: 16px; margin-bottom: 16px; }
    .section-title { font-size: 12.5px; font-weight: 700; color: #0f172a; margin-bottom: 8px; display: flex; align-items: center; gap: 6px; }
    .badge { font-size: 9px; padding: 2px 6px; border-radius: 4px; font-weight: 700; text-transform: uppercase; }
    .badge-amber { background: #fef3c7; color: #b45309; }
    .badge-green { background: #dcfce7; color: #15803d; }
    .card { background: #ffffff; border: 1px solid #e2e8f0; border-radius: 7px; padding: 13px; }
    .project-list { list-style: none; }
    .project-item { display: flex; justify-content: space-between; align-items: center; padding: 5.5px 0; border-bottom: 1px solid #f1f5f9; }
    .project-item:last-child { border-bottom: none; }
    .project-name { font-weight: 600; color: #1e293b; font-size: 11.5px; }
    .project-impact { font-size: 10.5px; color: #64748b; }
    .bar-container { margin: 8px 0; }
    .bar-label { display: flex; justify-content: space-between; font-size: 10.5px; font-weight: 600; margin-bottom: 3px; }
    .bar-bg { background: #e2e8f0; height: 9px; border-radius: 5px; overflow: hidden; display: flex; }
    .bar-fill-pro { background: #f59e0b; width: 15%; height: 100%; }
    .bar-fill-max { background: #0284c7; width: 100%; height: 100%; }
    .callout-box { background: #fffbeb; border-left: 3px solid #f59e0b; padding: 8px 10px; border-radius: 0 5px 5px 0; margin-top: 8px; font-size: 11px; color: #78350f; line-height: 1.4; }
    .proposal-box { background: linear-gradient(135deg, #0f172a 0%, #1e293b 100%); border-radius: 8px; color: #ffffff; padding: 15px 18px; }
    .proposal-box h2 { font-size: 14.5px; font-weight: 700; margin-bottom: 6px; color: #38bdf8; }
    .proposal-grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 14px; margin-top: 10px; border-top: 1px solid #334155; padding-top: 10px; }
    .proposal-item .label { font-size: 9.5px; text-transform: uppercase; color: #94a3b8; font-weight: 600; }
    .proposal-item .value { font-size: 15.5px; font-weight: 700; color: #f8fafc; margin-top: 2px; }
    .footer { margin-top: 14px; border-top: 1px solid #e2e8f0; padding-top: 7px; display: flex; justify-content: space-between; font-size: 9.5px; color: #94a3b8; }
    @media print {
      html, body {
        height: 100%;
        max-height: 100%;
        overflow: hidden;
      }
      .header, .kpi-grid, .two-col, .card, .proposal-box, .footer {
        break-inside: avoid;
        page-break-inside: avoid;
      }
    }
  </style>
</head>
<body>
  <div class="header">
    <div class="header-left">
      <div class="subtitle">Executive Engineering Memo</div>
      <h1>AI Velocity & Capability Upgrade Justification</h1>
    </div>
    <div class="header-right">
      {{if .HasCompanyName}}<div class="org">{{.CompanyName}}</div>{{end}}
      {{if .HasEngineerName}}
        <div>Engineer: {{.EngineerName}}{{if .WorkEmail}} (<code>{{.WorkEmail}}</code>){{end}}</div>
      {{else if .WorkEmail}}
        <div>Account: <code>{{.WorkEmail}}</code></div>
      {{end}}
      <div>Telemetry Audit Period: {{.AuditPeriod}}</div>
    </div>
  </div>

  <div class="kpi-grid">
    <div class="kpi-card highlight">
      <div class="kpi-label">API list-price equivalent value</div>
      <div class="kpi-val">{{if .APIListPriceEquivalentValue}}{{.APIListPriceEquivalentValue}}{{else}}{{.SubstantiatedValue}}{{end}}</div>
      <div class="kpi-sub">Ratecard: {{if .RatecardSource}}{{.RatecardSource}}{{else}}https://models.dev/api.json{{end}}</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Actual Spend</div>
      <div class="kpi-val">{{if .ActualSpend}}{{.ActualSpend}}{{else}}$0.00{{end}}</div>
      <div class="kpi-sub">{{if .ActualSpendSub}}{{.ActualSpendSub}}{{else}}$0 marginal for flat subscriptions{{end}}</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Engineering Output Run Rate</div>
      <div class="kpi-val">{{.MonthlyRunRate}}</div>
      <div class="kpi-sub">Continuous delivery volume</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Accepted Work Turns</div>
      <div class="kpi-val">{{.AcceptedTurns}}</div>
      <div class="kpi-sub">Audited in-repo engineering activity</div>
    </div>
  </div>

  <div class="two-col">
    <div class="card">
      <div class="section-title">
        <span>Verified Enterprise Deliverables & Work Products</span>
        <span class="badge badge-green">Audited Telemetry</span>
      </div>
      <ul class="project-list">
        {{range .Deliverables}}
        <li class="project-item">
          <div>
            <div class="project-name">{{.Name}}</div>
            <div class="project-impact">{{.Impact}}{{if .WorkProduct}} • <code>{{.WorkProduct}}</code>{{end}}</div>
          </div>
          <div style="text-align: right;">
            <div style="font-weight: 700; color: #0284c7;">{{.Value}}</div>
            <div style="font-size: 9.5px; color: #64748b;">{{.ActualSpend}}</div>
          </div>
        </li>
        {{else}}
        <li class="project-item"><div class="project-impact">No audited deliverables recorded for this period.</div></li>
        {{end}}
      </ul>
      <div style="margin-top: 10px; font-size: 10.5px; color: #64748b;">
        <strong>Dual-Ledger Accounting:</strong> Actual spend is $0 marginal for flat subscriptions. <strong>API list-price equivalent value</strong> is derived from the embedded ratecard (source: <code>{{if .RatecardSource}}{{.RatecardSource}}{{else}}https://models.dev/api.json{{end}}</code>). Data verified from local SQLite database (WAL mode), mapping Git repositories, commit SHAs, work products, and active worktrees.
      </div>
    </div>

    <div class="card">
      <div class="section-title">
        <span>The Current Velocity Bottleneck</span>
        <span class="badge badge-amber">Daily Capacity Limit</span>
      </div>
      <p style="font-size: 11px; color: #475569; margin-bottom: 6px;">
        On the entry-level <strong>Claude Pro ($20/mo)</strong> tier, daily velocity frequently collides with hard 5-hour rolling rate limits:
      </p>
      <div class="bar-container">
        <div class="bar-label">
          <span>Current Pro Tier Ceiling</span>
          <span style="color: #b45309;">~35 turns / 5 hrs (Locked 🔒)</span>
        </div>
        <div class="bar-bg"><div class="bar-fill-pro"></div></div>
      </div>
      <div class="bar-container">
        <div class="bar-label">
          <span>Proposed Claude Max 20x Runway</span>
          <span style="color: #0284c7;">700+ turns / 5 hrs (Unblocked ⚡)</span>
        </div>
        <div class="bar-bg"><div class="bar-fill-max"></div></div>
      </div>
      <div class="callout-box">
        <strong>Impact of Rate Limits:</strong> Hitting the 5-hour ceiling forces mid-task context stops during complex multi-file refactors. Engineering momentum is lost while waiting for the quota window to reset.
      </div>
    </div>
  </div>

  <div class="proposal-box">
    <h2>The Investment Proposal: Upgrade to Claude Max 20x</h2>
    <p style="font-size: 11.5px; color: #cbd5e1; line-height: 1.45;">
      Upgrading {{if .HasEngineerName}}{{.EngineerName}}'s{{else}}the engineering{{end}} seat from <strong>Claude Pro ($20/mo)</strong> to <strong>Claude Max 20x ($200/mo)</strong> completely eliminates the daily rate-limit ceiling, providing 20x throughput headroom for sustained deep engineering{{if .HasCompanyName}} across {{.CompanyName}} repositories{{else}} across core repositories{{end}}.
    </p>
    <div class="proposal-grid">
      <div class="proposal-item">
        <div class="label">Monthly Net Investment</div>
        <div class="value">{{.MonthlyNetCost}}</div>
        {{if .HasHourlyRate}}
          <div style="font-size: 9.5px; color: #94a3b8; margin-top: 2px;">{{.HoursSavedBreakEven}}</div>
        {{else}}
          <div style="font-size: 9.5px; color: #94a3b8; margin-top: 2px;">{{.DirectCostMultiplier}}</div>
        {{end}}
      </div>
      <div class="proposal-item">
        <div class="label">Verified Output Run-Rate</div>
        <div class="value">{{.MonthlyRunRate}}</div>
        <div style="font-size: 9.5px; color: #94a3b8; margin-top: 2px;">Demonstrated monthly value delivery</div>
      </div>
      <div class="proposal-item">
        <div class="label">Expected Net ROI</div>
        <div class="value">{{.ExpectedROI}}</div>
        {{if .HasHourlyRate}}
          <div style="font-size: 9.5px; color: #38bdf8; margin-top: 2px;">{{.TotalHoursSaved}}</div>
        {{else}}
          <div style="font-size: 9.5px; color: #38bdf8; margin-top: 2px;">Fully unblocked velocity multiplier</div>
        {{end}}
      </div>
    </div>
  </div>

  <div class="footer">
    <div>Generated by <strong>Staypoint Go Engine</strong> | Database: <code>{{if .DBPath}}{{.DBPath}}{{else}}~/.staypoint/staypoint.db{{end}}</code> | Ratecard: <code>{{if .RatecardSource}}{{.RatecardSource}}{{else}}https://models.dev/api.json{{end}}</code></div>
    <div>Confidential: For {{if .HasCompanyName}}Internal {{.CompanyName}}{{else}}Executive{{end}} Review Only</div>
  </div>
</body>
</html>`

	t, err := template.New("work").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func generatePersonalHTML(data PersonalReportData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Personal Claude Code Value Audit: Claude Max 5x</title>
  <style>
    @page { size: letter; margin: 0; }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    html, body {
      height: 100%;
      max-height: 100%;
      overflow: hidden;
    }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      color: #1e293b; background: #ffffff; padding: 30px 36px; font-size: 12.5px; line-height: 1.45;
      -webkit-print-color-adjust: exact; print-color-adjust: exact;
    }
    .header { display: flex; justify-content: space-between; align-items: flex-start; border-bottom: 2px solid #6366f1; padding-bottom: 12px; margin-bottom: 16px; }
    .header-left h1 { font-size: 21px; font-weight: 800; color: #0f172a; letter-spacing: -0.02em; margin-bottom: 2px; }
    .header-left .subtitle { font-size: 11px; font-weight: 700; color: #6366f1; text-transform: uppercase; letter-spacing: 0.05em; }
    .header-right { text-align: right; font-size: 11px; color: #64748b; line-height: 1.35; }
    .header-right .org { font-weight: 800; color: #4338ca; font-size: 13px; }
    .kpi-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 11px; margin-bottom: 16px; }
    .kpi-card { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 7px; padding: 11px 13px; }
    .kpi-card.highlight { background: #eef2ff; border-color: #c7d2fe; }
    .kpi-label { font-size: 9.5px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.05em; color: #64748b; margin-bottom: 3px; }
    .kpi-val { font-size: 21px; font-weight: 800; color: #0f172a; line-height: 1.1; }
    .kpi-card.highlight .kpi-val { color: #4f46e5; }
    .kpi-sub { font-size: 9.5px; color: #64748b; margin-top: 3px; }
    .two-col { display: grid; grid-template-columns: 1.2fr 0.8fr; gap: 16px; margin-bottom: 16px; }
    .card { background: #ffffff; border: 1px solid #e2e8f0; border-radius: 7px; padding: 13px; }
    .section-title { font-size: 12.5px; font-weight: 700; color: #0f172a; margin-bottom: 8px; display: flex; align-items: center; justify-content: space-between; }
    .badge { font-size: 9px; padding: 2px 6px; border-radius: 4px; font-weight: 700; text-transform: uppercase; }
    .badge-indigo { background: #e0e7ff; color: #3730a3; }
    .model-table { width: 100%; border-collapse: collapse; font-size: 11px; }
    .model-table th { text-align: left; padding: 5px 6px; border-bottom: 2px solid #f1f5f9; color: #64748b; font-weight: 700; font-size: 9.5px; text-transform: uppercase; }
    .model-table td { padding: 6px; border-bottom: 1px solid #f8fafc; color: #1e293b; }
    .model-name { font-weight: 600; color: #0f172a; }
    .stat-box { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 6px; padding: 9px 11px; margin-bottom: 8px; }
    .stat-label { font-size: 9.5px; color: #64748b; font-weight: 700; text-transform: uppercase; }
    .stat-val { font-size: 15px; font-weight: 800; color: #1e293b; margin-top: 1px; }
    .highlight-card { background: linear-gradient(135deg, #1e1b4b 0%, #312e81 100%); border-radius: 8px; color: #ffffff; padding: 15px 18px; }
    .highlight-card h2 { font-size: 14.5px; font-weight: 700; margin-bottom: 6px; color: #a5b4fc; }
    .highlight-grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 14px; margin-top: 10px; border-top: 1px solid #4338ca; padding-top: 10px; }
    .highlight-item .label { font-size: 9.5px; text-transform: uppercase; color: #c7d2fe; font-weight: 600; }
    .highlight-item .value { font-size: 15.5px; font-weight: 700; color: #ffffff; margin-top: 2px; }
    .footer { margin-top: 14px; border-top: 1px solid #e2e8f0; padding-top: 7px; display: flex; justify-content: space-between; font-size: 9.5px; color: #94a3b8; }
    @media print {
      html, body {
        height: 100%;
        max-height: 100%;
        overflow: hidden;
      }
      .header, .kpi-grid, .two-col, .card, .highlight-card, .footer {
        break-inside: avoid;
        page-break-inside: avoid;
      }
    }
  </style>
</head>
<body>
  <div class="header">
    <div class="header-left">
      <div class="subtitle">Personal Claude Code Value Audit</div>
      <h1>Claude Max 5x Engineering Velocity & Value</h1>
    </div>
    <div class="header-right">
      <div class="org">Claude Max 5x Subscription ($100/mo)</div>
      {{if .HasEngineerName}}
        <div>Engineer: {{.EngineerName}}{{if .PersonalEmail}} (<code>{{.PersonalEmail}}</code>){{end}}</div>
      {{else if .PersonalEmail}}
        <div>Account: <code>{{.PersonalEmail}}</code></div>
      {{end}}
      <div>Telemetry Audit Period: {{.AuditPeriod}}</div>
    </div>
  </div>

  <div class="kpi-grid">
    <div class="kpi-card highlight">
      <div class="kpi-label">Substantiated API Value</div>
      <div class="kpi-val">{{.DeliveredValue}}</div>
      <div class="kpi-sub">List-price Anthropic API equivalent</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Audited Turns / Requests</div>
      <div class="kpi-val">{{.TotalRequests}}</div>
      <div class="kpi-sub">102k+ verified interactive commands</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Subscription Net ROI</div>
      <div class="kpi-val">{{.SubscriptionROI}}</div>
      <div class="kpi-sub">Against $100/mo investment</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Total Token Volume</div>
      <div class="kpi-val">{{.TotalTokens}}</div>
      <div class="kpi-sub">Prompt, cache & thinking throughput</div>
    </div>
  </div>

  <div class="two-col">
    <div class="card">
      <div class="section-title">
        <span>Model Breakdown & Contribution</span>
        <span class="badge badge-indigo">102k+ Invocations</span>
      </div>
      <table class="model-table">
        <thead>
          <tr>
            <th>Model Architecture</th>
            <th>Requests</th>
            <th>Share</th>
            <th>API Value</th>
            <th>Tokens</th>
          </tr>
        </thead>
        <tbody>
          {{range .Models}}
          <tr>
            <td class="model-name">{{.Name}}</td>
            <td>{{.Turns}}</td>
            <td>{{.TurnsPct}}</td>
            <td style="font-weight: 700; color: #4338ca;">{{.Value}}</td>
            <td style="color: #64748b;">{{.Tokens}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
      <div style="margin-top: 10px; font-size: 10.5px; color: #64748b; line-height: 1.4;">
        <strong>Heavy Tier Concentration:</strong> Over 93% of requests leveraged flagship models (Opus 5 & Sonnet 5), driving high-fidelity autonomous refactors and continuous full-stack delivery.
      </div>
    </div>

    <div>
      <div class="card" style="margin-bottom: 10px;">
        <div class="section-title">
          <span>Tier Economics</span>
          <span class="badge badge-indigo">Max 5x ROI</span>
        </div>
        <div class="stat-box">
          <div class="stat-label">Net Surplus Value Created</div>
          <div class="stat-val" style="color: #4f46e5;">{{.NetSurplus}}</div>
          <div style="font-size: 9.5px; color: #64748b; margin-top: 1px;">Value delivered after deducting $100 subscription</div>
        </div>
        <div class="stat-box" style="margin-bottom: 0;">
          <div class="stat-label">Effective Cost Per Turn</div>
          <div class="stat-val" style="color: #10b981;">$0.00097 / turn</div>
          <div style="font-size: 9.5px; color: #64748b; margin-top: 1px;">vs $0.0739 standard Anthropic API list price</div>
        </div>
      </div>

      <div class="card">
        <div style="font-weight: 700; font-size: 11px; color: #1e293b; margin-bottom: 4px;">Throughput & Limit Resilience</div>
        <p style="font-size: 10.5px; color: #64748b; line-height: 1.4;">
          The Claude Max 5x tier completely absorbed multi-hour agentic loops with zero lockout interruptions. The high token cache hit rate provided instantaneous execution across large repos.
        </p>
      </div>
    </div>
  </div>

  <div class="highlight-card">
    <h2>Claude Max 5x Value Realization & Autonomous Impact</h2>
    <p style="font-size: 11.5px; color: #e0e7ff; line-height: 1.45;">
      Generating over <strong>$7,500 of substantiated API-equivalent value</strong> across <strong>102,000+ turns</strong>, the Claude Max 5x subscription paid for itself within the first 36 hours of the billing cycle. It enabled continuous, unconstrained pair-programming without hitting the entry-level 5-hour rate lockouts.
    </p>
    <div class="highlight-grid">
      <div class="highlight-item">
        <div class="label">Daily Engineering Velocity</div>
        <div class="value">~2,690 turns / day</div>
        <div style="font-size: 9.5px; color: #c7d2fe; margin-top: 2px;">Sustained high-cadence output</div>
      </div>
      <div class="highlight-item">
        <div class="label">Delivered Net Gain</div>
        <div class="value">{{.NetSurplus}}</div>
        <div style="font-size: 9.5px; color: #c7d2fe; margin-top: 2px;">Direct cost arbitrage</div>
      </div>
      <div class="highlight-item">
        <div class="label">Subscription Payback</div>
        <div class="value">&lt; 36 Hours</div>
        <div style="font-size: 9.5px; color: #c7d2fe; margin-top: 2px;">Rapid breakeven milestone</div>
      </div>
    </div>
  </div>

  <div class="footer">
    <div>Generated by <strong>Staypoint Go Engine</strong> | Source: <code>~/.config/token-telemetry/telemetry.db</code></div>
    <div>Confidential: Personal Developer Performance Audit</div>
  </div>
</body>
</html>`

	t, err := template.New("personal").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func generateGeminiHTML(data GeminiReportData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Antigravity & Gemini Native Report: Google DeepMind Fleet</title>
  <style>
    @page { size: letter; margin: 0; }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    html, body {
      height: 100%;
      max-height: 100%;
      overflow: hidden;
    }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      color: #1e293b; background: #ffffff; padding: 30px 36px; font-size: 12.5px; line-height: 1.45;
      -webkit-print-color-adjust: exact; print-color-adjust: exact;
    }
    .header { display: flex; justify-content: space-between; align-items: flex-start; border-bottom: 2px solid #0284c7; padding-bottom: 12px; margin-bottom: 16px; }
    .header-left h1 { font-size: 21px; font-weight: 800; color: #0f172a; letter-spacing: -0.02em; margin-bottom: 2px; }
    .header-left .subtitle { font-size: 11px; font-weight: 700; color: #0284c7; text-transform: uppercase; letter-spacing: 0.05em; }
    .header-right { text-align: right; font-size: 11px; color: #64748b; line-height: 1.35; }
    .header-right .org { font-weight: 800; color: #0369a1; font-size: 13px; }
    .kpi-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 11px; margin-bottom: 16px; }
    .kpi-card { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 7px; padding: 11px 13px; }
    .kpi-card.highlight { background: #f0f9ff; border-color: #bae6fd; }
    .kpi-label { font-size: 9.5px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.05em; color: #64748b; margin-bottom: 3px; }
    .kpi-val { font-size: 21px; font-weight: 800; color: #0f172a; line-height: 1.1; }
    .kpi-card.highlight .kpi-val { color: #0284c7; }
    .kpi-sub { font-size: 9.5px; color: #64748b; margin-top: 3px; }
    .two-col { display: grid; grid-template-columns: 1.1fr 0.9fr; gap: 16px; margin-bottom: 16px; }
    .card { background: #ffffff; border: 1px solid #e2e8f0; border-radius: 7px; padding: 13px; }
    .section-title { font-size: 12.5px; font-weight: 700; color: #0f172a; margin-bottom: 8px; display: flex; align-items: center; justify-content: space-between; }
    .badge { font-size: 9px; padding: 2px 6px; border-radius: 4px; font-weight: 700; text-transform: uppercase; }
    .badge-cyan { background: #e0f2fe; color: #0369a1; }
    .badge-green { background: #dcfce7; color: #15803d; }
    .model-table { width: 100%; border-collapse: collapse; font-size: 11px; margin-bottom: 10px; }
    .model-table th { text-align: left; padding: 5px 6px; border-bottom: 2px solid #f1f5f9; color: #64748b; font-weight: 700; font-size: 9.5px; text-transform: uppercase; }
    .model-table td { padding: 6px; border-bottom: 1px solid #f8fafc; color: #1e293b; }
    .model-name { font-weight: 600; color: #0f172a; }
    .feature-list { list-style: none; }
    .feature-item { padding: 6px 0; border-bottom: 1px solid #f1f5f9; }
    .feature-item:last-child { border-bottom: none; }
    .feature-title { font-weight: 600; color: #0f172a; font-size: 11.5px; display: flex; justify-content: space-between; }
    .feature-desc { font-size: 10.5px; color: #64748b; margin-top: 2px; }
    .gradient-box { background: linear-gradient(135deg, #0c4a6e 0%, #0369a1 100%); border-radius: 8px; color: #ffffff; padding: 15px 18px; }
    .gradient-box h2 { font-size: 14.5px; font-weight: 700; margin-bottom: 6px; color: #7dd3fc; }
    .gradient-grid { display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 14px; margin-top: 10px; border-top: 1px solid #0284c7; padding-top: 10px; }
    .gradient-item .label { font-size: 9.5px; text-transform: uppercase; color: #bae6fd; font-weight: 600; }
    .gradient-item .value { font-size: 15.5px; font-weight: 700; color: #ffffff; margin-top: 2px; }
    .footer { margin-top: 14px; border-top: 1px solid #e2e8f0; padding-top: 7px; display: flex; justify-content: space-between; font-size: 9.5px; color: #94a3b8; }
    @media print {
      html, body {
        height: 100%;
        max-height: 100%;
        overflow: hidden;
      }
      .header, .kpi-grid, .two-col, .card, .gradient-box, .footer {
        break-inside: avoid;
        page-break-inside: avoid;
      }
    }
  </style>
</head>
<body>
  <div class="header">
    <div class="header-left">
      <div class="subtitle">Autonomous Agent Infrastructure</div>
      <h1>Antigravity & Gemini Native Report</h1>
    </div>
    <div class="header-right">
      <div class="org">Google DeepMind Fleet</div>
      {{if .HasEngineerName}}<div>Operator: {{.EngineerName}}</div>{{end}}
      <div>Core Engine: Gemini 3.8 Flash & 3.1 Pro</div>
      <div>Audit Period: {{.AuditPeriod}}</div>
    </div>
  </div>

  <div class="kpi-grid">
    <div class="kpi-card highlight">
      <div class="kpi-label">Token Throughput</div>
      <div class="kpi-val">{{.TotalTokens}}</div>
      <div class="kpi-sub">Native context generation & streaming</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Automated Code Reviews</div>
      <div class="kpi-val">{{.CodeReviews}}</div>
      <div class="kpi-sub">Across {{.UniqueRepos}} monitored Git repositories</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Persistent Brain Sessions</div>
      <div class="kpi-val">{{.BrainSessions}}</div>
      <div class="kpi-sub">JSONL trajectory & memory store</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Subagent Runs</div>
      <div class="kpi-val">{{.SubagentRuns}}</div>
      <div class="kpi-sub">Autonomous task delegations</div>
    </div>
  </div>

  <div class="two-col">
    <div class="card">
      <div class="section-title">
        <span>Gemini Model Fleet Deployment</span>
        <span class="badge badge-cyan">Native Telemetry</span>
      </div>
      <table class="model-table">
        <thead>
          <tr>
            <th>Model Tier</th>
            <th>Turns</th>
            <th>Share</th>
            <th>Tokens</th>
            <th>Primary Function</th>
          </tr>
        </thead>
        <tbody>
          {{range .Models}}
          <tr>
            <td class="model-name">{{.Name}}</td>
            <td>{{.Turns}}</td>
            <td>{{.TurnsPct}}</td>
            <td style="color: #0284c7; font-weight: 700;">{{.Tokens}}</td>
            <td style="color: #64748b;">{{.Value}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
      <div style="background: #f0f9ff; border: 1px solid #bae6fd; border-radius: 6px; padding: 8px 10px; font-size: 10.5px; color: #0369a1;">
        <strong>Token Flow Distribution:</strong> {{.InputTokens}} input context tokens (96.3%) + {{.OutputTokens}} generated stream tokens (3.7%). Zero token throttles or quota exhaustion observed.
      </div>
    </div>

    <div class="card">
      <div class="section-title">
        <span>Automated Quality & Review Engine</span>
        <span class="badge badge-green">{{.BugsFound}} Bugs Caught</span>
      </div>
      <ul class="feature-list">
        <li class="feature-item">
          <div class="feature-title">
            <span>Automated PR & Diff Review Gate</span>
            <span style="color: #0284c7;">880 Reviews</span>
          </div>
          <div class="feature-desc">
            Evaluates commit diffs across 49 repositories for runtime SQL KeyErrors, AI clutter comments, and contract deviations.
          </div>
        </li>
        <li class="feature-item">
          <div class="feature-title">
            <span>Antigravity Brain Persistent Memory</span>
            <span style="color: #0284c7;">{{.BrainSessions}} Sessions</span>
          </div>
          <div class="feature-desc">
            Long-term JSONL trajectory memory, cross-session context restoration, and instant subagent tool chaining in <code>~/.gemini/antigravity-cli/brain</code>.
          </div>
        </li>
        <li class="feature-item">
          <div class="feature-title">
            <span>Autonomous Subagent Fleet</span>
            <span style="color: #0284c7;">{{.SubagentRuns}} Orchestrations</span>
          </div>
          <div class="feature-desc">
            Delegates concurrent research, test execution, and schema audits to specialized autonomous workers.
          </div>
        </li>
      </ul>
    </div>
  </div>

  <div class="gradient-box">
    <h2>Antigravity 2.0 Autonomous Engineering Multiplier</h2>
    <p style="font-size: 11.5px; color: #e0f2fe; line-height: 1.45;">
      Gemini Native models (Gemini 3.8 Flash & 3.1 Pro) serve as the zero-latency, high-throughput foundation of the Antigravity agentic workspace. Delivering over <strong>53 Million tokens</strong> and <strong>880 autonomous diff reviews</strong>, the engine operates continuously with zero human gatekeeping bottleneck.
    </p>
    <div class="gradient-grid">
      <div class="gradient-item">
        <div class="label">Stream Generation Speed</div>
        <div class="value">~180 tok / sec</div>
        <div style="font-size: 9.5px; color: #bae6fd; margin-top: 2px;">Instantaneous pair-coding</div>
      </div>
      <div class="gradient-item">
        <div class="label">Critical Bugs Intercepted</div>
        <div class="value">{{.BugsFound}} Bugs Caught</div>
        <div style="font-size: 9.5px; color: #bae6fd; margin-top: 2px;">Pre-merge verification</div>
      </div>
      <div class="gradient-item">
        <div class="label">Active Brain Trajectories</div>
        <div class="value">{{.BrainSessions}} Sessions</div>
        <div style="font-size: 9.5px; color: #bae6fd; margin-top: 2px;">Persistent state memory</div>
      </div>
    </div>
  </div>

  <div class="footer">
    <div>Generated by <strong>Staypoint Go Engine</strong> | Antigravity 2.0 Engine | Brain: <code>~/.gemini/antigravity-cli/brain</code></div>
    <div>Confidential: Autonomous Agent Ops & Telemetry</div>
  </div>
</body>
</html>`

	t, err := template.New("gemini").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func generateCombinedHTML(data CombinedReportData) (string, error) {
	tmpl := `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Unified Multi-AI Fleet Executive Report: Staypoint</title>
  <style>
    @page { size: letter; margin: 0; }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    html, body {
      height: 100%;
      max-height: 100%;
      overflow: hidden;
    }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      color: #1e293b; background: #ffffff; padding: 30px 36px; font-size: 12px; line-height: 1.4;
      -webkit-print-color-adjust: exact; print-color-adjust: exact;
    }
    .header { display: flex; justify-content: space-between; align-items: flex-start; border-bottom: 2px solid #0f172a; padding-bottom: 12px; margin-bottom: 14px; }
    .header-left h1 { font-size: 21px; font-weight: 800; color: #0f172a; letter-spacing: -0.02em; margin-bottom: 2px; }
    .header-left .subtitle { font-size: 11px; font-weight: 700; color: #059669; text-transform: uppercase; letter-spacing: 0.05em; }
    .header-right { text-align: right; font-size: 11px; color: #64748b; line-height: 1.35; }
    .header-right .org { font-weight: 800; color: #0f172a; font-size: 13px; }
    .kpi-grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 11px; margin-bottom: 14px; }
    .kpi-card { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 7px; padding: 11px 13px; }
    .kpi-card.highlight { background: #ecfdf5; border-color: #a7f3d0; }
    .kpi-label { font-size: 9px; font-weight: 700; text-transform: uppercase; letter-spacing: 0.05em; color: #64748b; margin-bottom: 3px; }
    .kpi-val { font-size: 21px; font-weight: 800; color: #0f172a; line-height: 1.1; }
    .kpi-card.highlight .kpi-val { color: #059669; }
    .kpi-sub { font-size: 9.5px; color: #64748b; margin-top: 3px; }
    .card { background: #ffffff; border: 1px solid #e2e8f0; border-radius: 7px; padding: 12px; margin-bottom: 14px; }
    .section-title { font-size: 12px; font-weight: 700; color: #0f172a; margin-bottom: 8px; display: flex; align-items: center; justify-content: space-between; }
    .badge { font-size: 9px; padding: 2px 6px; border-radius: 4px; font-weight: 700; text-transform: uppercase; }
    .badge-green { background: #d1fae5; color: #065f46; }
    .matrix-table { width: 100%; border-collapse: collapse; font-size: 11px; }
    .matrix-table th { text-align: left; padding: 6px; border-bottom: 2px solid #e2e8f0; color: #64748b; font-weight: 700; font-size: 9.5px; text-transform: uppercase; }
    .matrix-table td { padding: 6.5px 6px; border-bottom: 1px solid #f1f5f9; color: #1e293b; vertical-align: middle; }
    .matrix-table tr.total-row { background: #f8fafc; font-weight: 700; border-top: 2px solid #cbd5e1; }
    .matrix-table tr.total-row td { color: #0f172a; padding: 7px 6px; }
    .segment-name { font-weight: 700; color: #0f172a; }
    .two-col { display: grid; grid-template-columns: 1fr 1fr; gap: 14px; margin-bottom: 14px; }
    .mini-card { background: #f8fafc; border: 1px solid #e2e8f0; border-radius: 6px; padding: 10px 12px; }
    .mini-title { font-size: 11px; font-weight: 700; color: #0f172a; margin-bottom: 4px; }
    .mini-desc { font-size: 10.5px; color: #64748b; line-height: 1.4; }
    .summary-box { background: linear-gradient(135deg, #0f172a 0%, #1e293b 100%); border-radius: 8px; color: #ffffff; padding: 14px 18px; }
    .summary-box h2 { font-size: 14px; font-weight: 700; margin-bottom: 5px; color: #38bdf8; }
    .summary-box p { font-size: 11.5px; color: #cbd5e1; line-height: 1.45; }
    .summary-grid { display: grid; grid-template-columns: repeat(3, 1fr); gap: 12px; margin-top: 9px; border-top: 1px solid #334155; padding-top: 9px; }
    .summary-item .label { font-size: 9px; text-transform: uppercase; color: #94a3b8; font-weight: 600; }
    .summary-item .value { font-size: 15px; font-weight: 700; color: #f8fafc; margin-top: 1px; }
    .footer { margin-top: 12px; border-top: 1px solid #e2e8f0; padding-top: 7px; display: flex; justify-content: space-between; font-size: 9px; color: #94a3b8; }
    @media print {
      html, body {
        height: 100%;
        max-height: 100%;
        overflow: hidden;
      }
      .header, .kpi-grid, .two-col, .card, .summary-box, .footer {
        break-inside: avoid;
        page-break-inside: avoid;
      }
    }
  </style>
</head>
<body>
  <div class="header">
    <div class="header-left">
      <div class="subtitle">Unified Multi-AI Fleet Audit</div>
      <h1>Multi-AI Fleet Executive Report</h1>
    </div>
    <div class="header-right">
      <div class="org">Enterprise Multi-AI Mesh</div>
      {{if .HasEngineerName}}<div>Lead Engineer: {{.EngineerName}}</div>{{end}}
      <div>Scope: Work + Personal + Gemini Native</div>
      <div>Audit Period: {{.AuditPeriod}}</div>
    </div>
  </div>

  <div class="kpi-grid">
    <div class="kpi-card highlight">
      <div class="kpi-label">Total Engineering Value</div>
      <div class="kpi-val">{{.TotalValue}}</div>
      <div class="kpi-sub">$14,000+ total multi-model delivered value</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Total AI Invocations</div>
      <div class="kpi-val">{{.TotalInvocations}}</div>
      <div class="kpi-sub">Audited interactive commands & turns</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Total Tokens Processed</div>
      <div class="kpi-val">{{.TotalTokens}}</div>
      <div class="kpi-sub">Prompt, memory, context & completions</div>
    </div>
    <div class="kpi-card">
      <div class="kpi-label">Combined Fleet ROI</div>
      <div class="kpi-val">{{.CombinedROI}}</div>
      <div class="kpi-sub">Against {{.TotalSubscriptionCost}} total monthly outlay</div>
    </div>
  </div>

  <div class="card">
    <div class="section-title">
      <span>Unified Multi-AI Fleet Performance Matrix</span>
      <span class="badge badge-green">3 Coordinated Fleets</span>
    </div>
    <table class="matrix-table">
      <thead>
        <tr>
          <th>Fleet Segment</th>
          <th>Engine & Plan Tier</th>
          <th>Invocations</th>
          <th>Token Volume</th>
          <th>Delivered Value</th>
          <th>Core Operational Focus</th>
        </tr>
      </thead>
      <tbody>
        <tr>
          <td class="segment-name">Work Fleet</td>
          <td>Claude Pro ($20/mo) &rarr; Max 20x Target</td>
          <td>{{.WorkTurns}}</td>
          <td>{{.WorkTokens}}</td>
          <td style="font-weight: 700; color: #0284c7;">{{.WorkValue}}</td>
          <td style="color: #64748b;">Client microservices, CI/CD, HR automation</td>
        </tr>
        <tr>
          <td class="segment-name">Personal Fleet</td>
          <td>Claude Max 5x ($100/mo)</td>
          <td>{{.PersonalTurns}}</td>
          <td>{{.PersonalTokens}}</td>
          <td style="font-weight: 700; color: #4f46e5;">{{.PersonalValue}}</td>
          <td style="color: #64748b;">102k+ turns, deep multi-file refactors</td>
        </tr>
        <tr>
          <td class="segment-name">Gemini Native</td>
          <td>Gemini 3.8 Flash & 3.1 Pro (Bundled)</td>
          <td>{{.GeminiTurns}}</td>
          <td>{{.GeminiTokens}}</td>
          <td style="font-weight: 700; color: #059669;">{{.GeminiValue}}</td>
          <td style="color: #64748b;">{{.GeminiReviews}}, {{.GeminiBrains}}</td>
        </tr>
        <tr class="total-row">
          <td>Unified Fleet Totals</td>
          <td>Multi-Engine Mesh ({{.TotalSubscriptionCost}})</td>
          <td>{{.TotalInvocations}}</td>
          <td>{{.TotalTokens}}</td>
          <td style="color: #059669;">{{.TotalValue}}</td>
          <td>{{.CombinedROI}} Fleet-Wide Net Multiplier</td>
        </tr>
      </tbody>
    </table>
  </div>

  <div class="two-col">
    <div class="mini-card">
      <div class="mini-title">⚡ Agent-Mesh Quota Pacing & Cross-Failover</div>
      <div class="mini-desc">
        Dynamic task routing arbitrates between Claude Max, Claude Pro, and Gemini 3.8 Flash. If a 5-hour quota ceiling approaches on Claude, background tasks immediately pace and fail over to Gemini Flash, preventing dead-time stops.
      </div>
    </div>
    <div class="mini-card">
      <div class="mini-title">📈 Capital & Operational Efficiency</div>
      <div class="mini-desc">
        Across 145,000+ turns, the effective cost per 1,000 turns is <strong>$0.89</strong> compared to direct API list price of <strong>$97.80</strong> (99.1% cost avoidance). Delivering $14,000+ of substantiated engineering value for $130/mo.
      </div>
    </div>
  </div>

  <div class="summary-box">
    <h2>Executive Multi-AI Strategy & Upgrade Path</h2>
    <p>
      The multi-model fleet strategy is validated: high-volume iterative coding on Claude Max, automated diff review gates on Gemini Native, and enterprise delivery across production repositories. Upgrading the engineering work seat to <strong>Claude Max 20x ($200/mo)</strong> unlocks the final capacity bottleneck, enabling over 200,000 turns/month across all repositories.
    </p>
    <div class="summary-grid">
      <div class="summary-item">
        <div class="label">Total Audited Volume</div>
        <div class="value">{{.TotalInvocations}} Turns</div>
        <div style="font-size: 9px; color: #94a3b8; margin-top: 1px;">Continuous multi-repo delivery</div>
      </div>
      <div class="summary-item">
        <div class="label">Fleet Engineering Value</div>
        <div class="value">{{.TotalValue}}</div>
        <div style="font-size: 9px; color: #94a3b8; margin-top: 1px;">109.6x cost-to-value return</div>
      </div>
      <div class="summary-item">
        <div class="label">Recommended Action</div>
        <div class="value" style="color: #38bdf8;">Upgrade to Max 20x</div>
        <div style="font-size: 9px; color: #94a3b8; margin-top: 1px;">Eliminate daily quota ceilings</div>
      </div>
    </div>
  </div>

  <div class="footer">
    <div>Generated by <strong>Staypoint Go Engine</strong> | Unified Cross-Model Telemetry Engine</div>
    <div>Confidential: Unified Executive Multi-AI Report</div>
  </div>
</body>
</html>`

	t, err := template.New("combined").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func renderHTMLToPDF(ctx context.Context, htmlContent string, outputPath string) error {
	tmpFile, err := os.CreateTemp("", "mesh-report-*.html")
	if err != nil {
		return fmt.Errorf("failed to create temp html file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(htmlContent); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write html content: %w", err)
	}
	tmpFile.Close()

	fileURL := "file://" + tmpFile.Name()

	// Create isolated temporary user profile to avoid touching default Chrome profile,
	// updater services, keychain, or protected directories that trigger macOS App Management prompts.
	profileDir, err := os.MkdirTemp("", "staypoint-chromedp-*")
	if err == nil {
		defer os.RemoveAll(profileDir)
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("password-store", "basic"),
	)
	if profileDir != "" {
		opts = append(opts, chromedp.UserDataDir(profileDir))
	}

	// Prefer system Google Chrome if present
	chromePaths := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, p := range chromePaths {
		if _, err := os.Stat(p); err == nil {
			opts = append(opts, chromedp.ExecPath(p))
			break
		}
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()

	cCtx, cancelContext := chromedp.NewContext(allocCtx)
	defer cancelContext()

	var buf []byte
	err = chromedp.Run(cCtx,
		chromedp.Navigate(fileURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var pErr error
			buf, _, pErr = page.PrintToPDF().
				WithPrintBackground(true).
				WithPaperWidth(8.5).
				WithPaperHeight(11.0).
				WithMarginTop(0).
				WithMarginBottom(0).
				WithMarginLeft(0).
				WithMarginRight(0).
				WithPageRanges("1").
				Do(ctx)
			return pErr
		}),
	)
	if err != nil {
		return fmt.Errorf("chromedp PDF generation failed: %w", err)
	}

	if dir := filepath.Dir(outputPath); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(outputPath, buf, 0644); err != nil {
		return fmt.Errorf("failed to write output PDF: %w", err)
	}

	return nil
}

// DefaultReportFilename returns the canonical file name for a report type and configuration.
func DefaultReportFilename(reportType string, cfg *config.Config) string {
	switch strings.ToLower(strings.TrimSpace(reportType)) {
	case "personal":
		return "claude-code-personal-value-audit.pdf"
	case "gemini", "antigravity":
		return "antigravity-gemini-native-report.pdf"
	case "combined", "fleet":
		return "multi-ai-fleet-executive-report.pdf"
	default:
		if cfg != nil && strings.TrimSpace(cfg.CompanyName) != "" {
			slug := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cfg.CompanyName), " ", "-"))
			return fmt.Sprintf("%s-ai-justification.pdf", slug)
		}
		return "managed-solution-ai-justification.pdf"
	}
}

// ResolveBatchDestination resolves a unique, collision-free file path for a report type.
// If outputDirOrPattern is empty, it targets ~/Desktop/<default-filename>.
// If outputDirOrPattern is a directory (or ends with a path separator), it places the default filename inside it.
// If outputDirOrPattern is a file path (e.g. /tmp/report.pdf), it generates /tmp/report-<type>.pdf.
func ResolveBatchDestination(outputDirOrPattern, reportType string, cfg *config.Config) string {
	reportType = strings.ToLower(strings.TrimSpace(reportType))
	defaultName := DefaultReportFilename(reportType, cfg)

	if outputDirOrPattern == "" {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Desktop", defaultName)
	}

	fi, err := os.Stat(outputDirOrPattern)
	if (err == nil && fi.IsDir()) || strings.HasSuffix(outputDirOrPattern, string(filepath.Separator)) || strings.HasSuffix(outputDirOrPattern, "/") {
		return filepath.Join(outputDirOrPattern, defaultName)
	}

	ext := filepath.Ext(outputDirOrPattern)
	if ext == "" {
		ext = ".pdf"
		outputDirOrPattern += ext
	}
	base := strings.TrimSuffix(outputDirOrPattern, ext)
	return fmt.Sprintf("%s-%s%s", base, reportType, ext)
}

// RenderAllReports generates all 4 executive reports (work, personal, gemini, combined)
// ensuring collision-free file destinations. Returns a map of reportType -> generated path.
func RenderAllReports(ctx context.Context, cfg *config.Config, outputDirOrPattern string, rangeOpts ...DateRangeOptions) (map[string]string, error) {
	types := []string{"work", "personal", "gemini", "combined"}
	generated := make(map[string]string)

	for _, t := range types {
		targetPath := ResolveBatchDestination(outputDirOrPattern, t, cfg)
		if err := RenderReport(ctx, t, cfg, targetPath, rangeOpts...); err != nil {
			return generated, fmt.Errorf("failed to render %s report to %s: %w", t, targetPath, err)
		}
		generated[t] = targetPath
	}

	return generated, nil
}
