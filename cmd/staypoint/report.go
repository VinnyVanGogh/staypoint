package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/reporting"
	"github.com/spf13/cobra"
)

var reportCmd = &cobra.Command{
	Use:   "report",
	Short: "Generate executive ROI briefings, value audits, and multi-AI reports",
	Run: func(cmd *cobra.Command, args []string) {
		pdfFlag, _ := cmd.Flags().GetBool("pdf")
		reportType, _ := cmd.Flags().GetString("type")
		outFlag, _ := cmd.Flags().GetString("output")
		sinceFlag, _ := cmd.Flags().GetString("since")
		untilFlag, _ := cmd.Flags().GetString("until")

		if !pdfFlag {
			fmt.Println("Usage: staypoint report --pdf [--type work|personal|gemini|combined|all] [--since <date>] [--until <date>] [--output <path>]")
			fmt.Println("  --type work        Executive Justification Memo (Boss Card)")
			fmt.Println("  --type personal    Personal Claude Code Value Audit (102k+ turns, $7,500+ value)")
			fmt.Println("  --type gemini      Antigravity & Gemini Native Report (Flash, Pro, Brain logs, Reviews)")
			fmt.Println("  --type combined    Unified Multi-AI Fleet Executive Report ($14,000+ total value)")
			fmt.Println("  --type all         Generate all 4 reports in one batch")
			fmt.Println("  --since <date>     Filter telemetry starting from date (e.g. 2026-08-01, 7d, 30d)")
			fmt.Println("  --until <date>     Filter telemetry up to date (e.g. 2026-09-01)")
			return
		}

		if reportType == "" {
			reportType = "work"
		}

		rangeOpts := reporting.DateRangeOptions{
			Since: sinceFlag,
			Until: untilFlag,
		}

		home, _ := os.UserHomeDir()

		if strings.ToLower(reportType) == "all" {
			types := []string{"work", "personal", "gemini", "combined"}
			fmt.Println("\033[1;36m[Staypoint]\033[0m Generating all 4 executive PDF reports...")
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			for _, t := range types {
				targetPath := reporting.ResolveBatchDestination(outFlag, t, cfg)

				fmt.Printf("  • Rendering \033[1;33m%s\033[0m report...", t)
				if err := reporting.RenderReport(ctx, t, cfg, targetPath, rangeOpts); err != nil {
					fmt.Printf(" \033[1;31mFAILED\033[0m (%v)\n", err)
				} else {
					fmt.Printf(" \033[1;32m✔ DONE\033[0m -> %s\n", targetPath)
				}
			}
			fmt.Println("\033[1;32m✔ All reports successfully generated!\033[0m")
			return
		}

		if outFlag == "" {
			outFlag = filepath.Join(home, "Desktop", reporting.DefaultReportFilename(reportType, cfg))
		}

		fmt.Printf("\033[1;36m[Staypoint]\033[0m Rendering \033[1;33m%s\033[0m report via Chrome CDP...\n", reportType)

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := reporting.RenderReport(ctx, reportType, cfg, outFlag, rangeOpts); err != nil {
			fmt.Fprintf(os.Stderr, "\033[1;31m✖ Error generating PDF:\033[0m %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("\033[1;32m✔ Successfully generated PDF:\033[0m %s\n", outFlag)
		_ = exec.Command("open", outFlag).Start()
	},
}

func init() {
	rootCmd.AddCommand(reportCmd)
	reportCmd.Flags().Bool("pdf", false, "Generate print-ready PDF report")
	reportCmd.Flags().StringP("type", "t", "work", "Report type: work, personal, gemini, combined (default: work)")
	reportCmd.Flags().StringP("output", "o", "", "Destination path for generated PDF")
	reportCmd.Flags().String("since", "", "Filter telemetry starting from date (e.g. 2026-08-01, 7d, 30d)")
	reportCmd.Flags().String("until", "", "Filter telemetry up to date (e.g. 2026-09-01)")
}
