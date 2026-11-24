package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/AsatovDias/trimbar/internal/anthropic"
	"github.com/AsatovDias/trimbar/internal/config"
	"github.com/AsatovDias/trimbar/internal/gitmeta"
	"github.com/AsatovDias/trimbar/internal/proxy"
	"github.com/AsatovDias/trimbar/internal/storage"
	"github.com/AsatovDias/trimbar/internal/webui"
)

var version = "0.4.3"

// Lipgloss styles for terminal output.
var (
	styleBrand = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F3F1ED"))
	styleLime  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C4F82A"))
	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("86"))
	styleMuted = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleOK    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#C4F82A"))
	styleWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	styleErr   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	styleBadge = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	styleSub   = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))

	styleBanner = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#3D4B24")).
			Padding(0, 2)

	styleLogoBox = lipgloss.NewStyle().
			Background(lipgloss.Color("#0E1015")).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#2A303D")).
			Padding(0, 1)

	styleCard = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 2).
			MarginRight(1)

	styleTableHeader = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("252")).
				BorderStyle(lipgloss.NormalBorder()).
				BorderBottom(true).
				BorderForeground(lipgloss.Color("240"))
)

// renderBrandHeader renders the signature 3-row Trimbar logo badge next to title & subtitle.
func renderBrandHeader(title, subtitle string) string {
	bg := lipgloss.Color("#0E1015")
	light := lipgloss.NewStyle().Foreground(lipgloss.Color("#F3F1ED")).Background(bg)
	dot := lipgloss.NewStyle().Foreground(lipgloss.Color("#454749")).Background(bg)
	lime := lipgloss.NewStyle().Foreground(lipgloss.Color("#C4F82A")).Background(bg)
	olive := lipgloss.NewStyle().Foreground(lipgloss.Color("#3D4B24")).Background(bg)
	gap := lipgloss.NewStyle().Background(bg)

	row1 := light.Render("━━━━━━━━━━━")
	row2 := light.Render("━━━━━━━━") + gap.Render("  ") + dot.Render("●")
	row3 := lime.Render("━━━━━") + gap.Render(" ") + olive.Render("━━━━━")

	logo := styleLogoBox.Render(strings.Join([]string{row1, row2, row3}, "\n"))
	wordmark := styleBrand.Render("TRIM") + styleLime.Render("BAR")
	if title != "" {
		wordmark += " " + styleBrand.Render(title)
	}
	textBlock := lipgloss.JoinVertical(lipgloss.Left,
		wordmark,
		styleMuted.Render(subtitle),
	)
	return lipgloss.JoinHorizontal(lipgloss.Center, logo, "  ", textBlock)
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, styleErr.Render("Error:"), err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "trimbar",
		Short: "Local-first Anthropic API reverse proxy and context telemetry CLI for Claude Code",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newStartCmd(),
		newStatsCmd(),
		newInspectCmd(),
		newDemoCmd(),
		newResetCmd(),
		newVersionCmd(),
	)
	return root
}

func newStartCmd() *cobra.Command {
	var (
		host        string
		port        int
		upstream    string
		dbPath      string
		mockMode    bool
		trimEnabled bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the local zero-buffer Anthropic reverse proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("host") {
				cfg.Host = host
			}
			if cmd.Flags().Changed("port") {
				cfg.Port = port
			}
			if cmd.Flags().Changed("upstream") {
				cfg.UpstreamURL = strings.TrimRight(upstream, "/")
			}
			if cmd.Flags().Changed("db") {
				cfg.DBPath = dbPath
			}
			if cmd.Flags().Changed("trim") {
				cfg.TrimEnabled = trimEnabled
			}

			if mockMode {
				workDir, _ := os.Getwd()
				mockSrv := httptest.NewServer(newMockAnthropicHandler(buildDemoTurns(workDir, "mock")))
				defer mockSrv.Close()
				cfg.UpstreamURL = mockSrv.URL
			}

			if err := cfg.EnsureDataDir(); err != nil {
				return fmt.Errorf("ensure data dir: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			repo := storage.NewRepository(db)
			resolver := gitmeta.NewResolver()

			onComplete := func(ev proxy.ProxyEvent) {
				rec := buildRequestLog(ev, resolver, repo)
				persistRequestLog(repo, rec, os.Stderr)
				printLiveEvent(rec)
			}

			onRetry := func(re proxy.RetryEvent) {
				ts := styleMuted.Render(time.Now().Format("15:04:05"))
				if re.Failover {
					fmt.Printf("%s %s HTTP %d -> failover #%d to %s\n",
						ts, styleWarn.Render("FAILOVER"), re.StatusCode, re.Attempt, re.TargetURL)
				} else {
					fmt.Printf("%s %s HTTP %d -> retry #%d after %s\n",
						ts, styleWarn.Render("RETRY   "), re.StatusCode, re.Attempt, re.Delay.Round(time.Millisecond))
				}
			}

			uiHandler := webui.NewHandler(repo)
			uiHandler.SetAllowedHost(cfg.Host)
			srv, err := proxy.NewServer(
				cfg,
				proxy.WithOnComplete(onComplete),
				proxy.WithOnRetry(onRetry),
				proxy.WithWebUI(uiHandler),
			)
			if err != nil {
				return fmt.Errorf("create proxy server: %w", err)
			}

			listenURL := fmt.Sprintf("http://%s", cfg.ListenAddr())
			webUIURL := listenURL + "/ui"
			usageHint := fmt.Sprintf("  ANTHROPIC_BASE_URL=%s claude", listenURL)
			upstreamLabel := cfg.UpstreamURL
			if mockMode {
				upstreamLabel = cfg.UpstreamURL + " " + styleWarn.Render("(local mock API)")
				usageHint = fmt.Sprintf("  trimbar demo --proxy %s", listenURL)
			}
			modeLabel := styleMuted.Render("Shadow Mode (read-only)")
			if cfg.TrimEnabled {
				modeLabel = styleOK.Render("Active Trimming (cache-safe)")
			}
			bannerLines := []string{
				renderBrandHeader("", "v"+version+" · Claude Code Local Gateway"),
				"",
				fmt.Sprintf("%s  %s", styleMuted.Render("Listen:  "), styleOK.Render(listenURL)),
				fmt.Sprintf("%s  %s", styleMuted.Render("Web UI:  "), styleTitle.Render(webUIURL)),
				fmt.Sprintf("%s  %s", styleMuted.Render("Upstream:"), upstreamLabel),
				fmt.Sprintf("%s  %s", styleMuted.Render("Mode:    "), modeLabel),
				fmt.Sprintf("%s  %s", styleMuted.Render("Database:"), cfg.DBPath),
				"",
				styleMuted.Render("Run Claude Code through Trimbar:"),
				styleTitle.Render(usageHint),
			}
			fmt.Println(styleBanner.Render(strings.Join(bannerLines, "\n")))
			if warn := nonLoopbackSecurityWarning(cfg.Host); warn != "" {
				fmt.Fprintln(os.Stderr, warn)
			}
			fmt.Println(styleMuted.Render("Press Ctrl+C to stop. Streaming telemetry below..."))
			fmt.Println()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if err := srv.Start(ctx); err != nil {
				return err
			}
			fmt.Println("\n" + styleMuted.Render("Trimbar stopped cleanly."))
			return nil
		},
	}

	cmd.Flags().StringVar(&host, "host", config.DefaultHost, "Host address to bind")
	cmd.Flags().IntVarP(&port, "port", "p", config.DefaultPort, "Port to listen on")
	cmd.Flags().StringVar(&upstream, "upstream", config.DefaultUpstreamURL, "Upstream Anthropic API URL")
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to SQLite database (default ~/.trimbar/trimbar.db)")
	cmd.Flags().BoolVar(&mockMode, "mock", false, "Use built-in local mock Anthropic SSE server (no API key needed)")
	cmd.Flags().BoolVar(&trimEnabled, "trim", false, "Enable cache-safe active trimming of post-cache_control tool_results (default false: Shadow Mode)")
	return cmd
}

func newStatsCmd() *cobra.Command {
	var (
		sinceStr  string
		repoName  string
		branch    string
		sessionID string
		dbPath    string
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Display aggregated token usage, prompt cache efficiency, and USD cost stats",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if dbPath != "" {
				cfg.DBPath = dbPath
			}

			since, err := parseSince(sinceStr)
			if err != nil {
				return err
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			repo := storage.NewRepository(db)
			stats, err := repo.GetStats(context.Background(), storage.StatsFilter{
				Since:     since,
				Repo:      repoName,
				Branch:    branch,
				SessionID: sessionID,
			})
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(stats)
			}

			renderStats(stats, sinceStr)
			return nil
		},
	}

	cmd.Flags().StringVar(&sinceStr, "since", "all", "Time window: e.g. 24h, 7d, 30d, all")
	cmd.Flags().StringVar(&repoName, "repo", "", "Filter by Git repository")
	cmd.Flags().StringVar(&branch, "branch", "", "Filter by Git branch")
	cmd.Flags().StringVar(&sessionID, "session", "", "Filter by session ID")
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to SQLite database")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output stats as JSON")
	return cmd
}

func newInspectCmd() *cobra.Command {
	var (
		sessionID string
		repoName  string
		branch    string
		limit     int
		dbPath    string
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Inspect context breakdown, duplicate Read calls, ANSI waste, and cache miss anomalies",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if dbPath != "" {
				cfg.DBPath = dbPath
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			repo := storage.NewRepository(db)
			report, err := repo.GetInspect(context.Background(), storage.StatsFilter{
				SessionID: sessionID,
				Repo:      repoName,
				Branch:    branch,
				Limit:     limit,
			})
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}

			renderInspect(report)
			return nil
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Filter by session ID")
	cmd.Flags().StringVar(&repoName, "repo", "", "Filter by Git repository")
	cmd.Flags().StringVar(&branch, "branch", "", "Filter by Git branch")
	cmd.Flags().IntVarP(&limit, "limit", "n", 15, "Number of recent turns to inspect")
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to SQLite database")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output report as JSON")
	return cmd
}

func newResetCmd() *cobra.Command {
	var (
		yes    bool
		dbPath string
	)

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Clear all recorded request logs from the local SQLite database",
		Long: `Clears all rows from the requests table in ~/.trimbar/trimbar.db (or --db).
Useful for wiping simulated trimbar demo records before connecting real Claude Code sessions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if dbPath != "" {
				cfg.DBPath = dbPath
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return err
			}
			defer db.Close()

			repo := storage.NewRepository(db)
			ctx := context.Background()
			count, err := repo.CountRequests(ctx)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if count == 0 {
				fmt.Fprintf(out, "%s\n", styleMuted.Render("Database is already empty (0 requests)."))
				return nil
			}

			if !yes {
				fmt.Fprintf(out, "Clear %d recorded request(s) from %s? [y/N]: ", count, cfg.DBPath)
				reader := bufio.NewReader(cmd.InOrStdin())
				line, _ := reader.ReadString('\n')
				ans := strings.ToLower(strings.TrimSpace(line))
				if ans != "y" && ans != "yes" {
					fmt.Fprintf(out, "%s\n", styleMuted.Render("Aborted."))
					return nil
				}
			}

			deleted, err := repo.ClearRequests(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "%s Cleared %d request(s) from %s.\n",
				styleOK.Render("✓"), deleted, cfg.DBPath)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip interactive confirmation prompt")
	cmd.Flags().StringVar(&dbPath, "db", "", "Path to SQLite database (default ~/.trimbar/trimbar.db)")
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print Trimbar version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("trimbar %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		},
	}
}

func buildRequestLog(ev proxy.ProxyEvent, resolver *gitmeta.Resolver, repo *storage.Repository) *storage.RequestLog {
	rec := &storage.RequestLog{
		Timestamp:   ev.Timestamp.UTC(),
		Method:      ev.Method,
		Path:        ev.Path,
		StatusCode:  ev.StatusCode,
		IsStreaming: ev.IsStreaming,
		LatencyMS:   ev.Latency.Milliseconds(),
		TTFBMS:      ev.TTFB.Milliseconds(),
		CostUSD:            ev.Cost.TotalCostUSD,
		CostEstimated:      ev.Cost.Estimated,
		CacheSavingsUSD:    ev.Cost.CacheSavingsUSD,
		NetCacheSavingsUSD: ev.Cost.NetCacheSavingsUSD,
		ShadowSavingsUSD:   ev.ShadowSavings.HonestSavingsUSD,
		TrimmedSavingsUSD:  ev.TrimmedSavingsUSD,
		RetryCount:         ev.RetryCount,
		FailoverUsed:       ev.FailoverUsed,
		ErrorBody:          ev.ErrorBody,
		AgentRole:          anthropic.AgentRoleMain,
	}

	if ev.TrimResult != nil {
		rec.TrimmedTokens = ev.TrimResult.TrimmedTokens
	}

	if ev.Inspection != nil {
		insp := ev.Inspection
		rec.Model = insp.Model
		rec.AgentRole = insp.AgentRole
		rec.CacheBreakpoints = insp.CacheBreakpoints
		rec.SystemEstTokens = insp.SystemEstTokens
		rec.ToolsEstTokens = insp.ToolsEstTokens
		rec.MessagesEstTokens = insp.MessagesEstTokens
		rec.ToolResultsEstTokens = insp.ToolResultsEstTokens
		rec.DuplicateReadCount = insp.DuplicateReadRedundantCalls
		rec.DuplicateReadWastedTokens = insp.DuplicateReadWastedTokens
		rec.DuplicateReads = insp.DuplicateReads
		rec.ANSISequenceCount = insp.ANSI.SequenceCount
		rec.ANSIWastedTokens = insp.ANSI.EstTokens
		rec.TotalWastedTokens = insp.TotalWastedEstTokens
		rec.PostCacheToolBytes = insp.PostCacheToolBytes

		if resolver != nil {
			attr := resolver.Resolve(insp)
			rec.CWD = attr.CWD
			rec.GitRepo = attr.Repo
			rec.GitBranch = attr.Branch
			rec.SessionID = attr.SessionID
		}
	}

	if ev.StreamResult != nil {
		sr := ev.StreamResult
		rec.RequestID = sr.MessageID
		if sr.Model != "" {
			rec.Model = sr.Model
		}
		rec.InputTokens = sr.Usage.InputTokens
		rec.OutputTokens = sr.Usage.OutputTokens
		rec.CacheCreationTokens = sr.Usage.CacheCreationInputTokens
		rec.CacheReadTokens = sr.Usage.CacheReadInputTokens

		// Real-time Cache Miss Anomaly detection for consecutive main-agent turns in the same session.
		if repo != nil && rec.SessionID != "" && rec.AgentRole == anthropic.AgentRoleMain &&
			rec.StatusCode >= 200 && rec.StatusCode < 300 {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			prevUsage, err := repo.LatestUsageForSession(ctx, rec.SessionID)
			cancel()
			if err == nil && prevUsage != nil {
				if anomaly := anthropic.DetectCacheMissAnomaly(*prevUsage, sr.Usage); anomaly != nil {
					rec.CacheMissAnomaly = true
					rec.CacheMissReason = fmt.Sprintf("%s (hit %.0f%% -> %.0f%%, +%s cache write)",
						anomaly.Reason,
						anomaly.PreviousHitRatio*100,
						anomaly.CurrentHitRatio*100,
						formatTokens(anomaly.CacheCreationSpike),
					)
				}
			}
		}
	}

	return rec
}

func printLiveEvent(rec *storage.RequestLog) {
	ts := styleMuted.Render(rec.Timestamp.Local().Format("15:04:05"))

	statusStr := styleOK.Render(fmt.Sprintf("%d", rec.StatusCode))
	if rec.StatusCode >= 400 {
		statusStr = styleErr.Render(fmt.Sprintf("%d", rec.StatusCode))
	} else if rec.StatusCode >= 300 {
		statusStr = styleWarn.Render(fmt.Sprintf("%d", rec.StatusCode))
	}

	roleStr := styleBadge.Render("[main]")
	if rec.AgentRole == anthropic.AgentRoleSubagent {
		roleStr = styleSub.Render("[sub] ")
	}

	model := shortModel(rec.Model)
	if model == "" {
		model = rec.Path
	}

	gitTag := ""
	if rec.GitRepo != "" {
		branch := rec.GitBranch
		if branch == "" {
			branch = "-"
		}
		gitTag = styleMuted.Render(fmt.Sprintf(" %s@%s", rec.GitRepo, branch))
	}

	var flags []string
	if rec.TrimmedTokens > 0 {
		flags = append(flags, styleOK.Render(fmt.Sprintf("TRIM:-%s", formatTokens(int64(rec.TrimmedTokens)))))
	}
	if rec.FailoverUsed {
		flags = append(flags, styleWarn.Render("FAILOVER"))
	} else if rec.RetryCount > 0 {
		flags = append(flags, styleWarn.Render(fmt.Sprintf("RETRY:%d", rec.RetryCount)))
	}
	if rec.CacheMissAnomaly {
		flags = append(flags, styleErr.Render("CACHE-MISS"))
	}
	if rec.DuplicateReadCount > 0 {
		flags = append(flags, styleWarn.Render(fmt.Sprintf("DUP-READ:%d", rec.DuplicateReadCount)))
	}
	if rec.ANSISequenceCount > 0 {
		flags = append(flags, styleWarn.Render(fmt.Sprintf("ANSI:%d", rec.ANSISequenceCount)))
	}
	flagStr := ""
	if len(flags) > 0 {
		flagStr = "  " + strings.Join(flags, " ")
	}

	fmt.Printf("%s %s %s %-18s%s  hit=%4.0f%%  in=%-6s out=%-5s  %s  %dms%s\n",
		ts,
		statusStr,
		roleStr,
		model,
		gitTag,
		rec.CacheHitRatio()*100,
		formatTokens(rec.TotalInputTokens()),
		formatTokens(rec.OutputTokens),
		styleOK.Render(formatUSD(rec.CostUSD, rec.CostEstimated)),
		rec.LatencyMS,
		flagStr,
	)
}

func formatUSD(amount float64, estimated bool) string {
	if estimated {
		return fmt.Sprintf("~$%.4f", amount)
	}
	return fmt.Sprintf("$%.4f", amount)
}

func renderStats(s *storage.StatsSummary, window string) {
	fmt.Println(styleBrand.Render("TRIMBAR STATS") + " " + styleMuted.Render("window="+window))
	fmt.Println()

	if s.TotalRequests == 0 {
		fmt.Println(styleMuted.Render("No requests recorded yet. Start Trimbar with `trimbar start` and run Claude Code."))
		return
	}

	// Top KPI cards
	card1 := styleCard.Render(fmt.Sprintf("%s\n%s\n%s",
		styleMuted.Render("REQUESTS"),
		styleTitle.Render(fmt.Sprintf("%d", s.TotalRequests)),
		styleMuted.Render(fmt.Sprintf("main %d · sub %d · err %d", s.MainAgentRequests, s.SubagentRequests, s.ErrorRequests)),
	))
	card2 := styleCard.Render(fmt.Sprintf("%s\n%s\n%s",
		styleMuted.Render("TOTAL SPEND"),
		styleOK.Render(formatUSD(s.TotalCostUSD, s.HasEstimatedCost)),
		styleMuted.Render(fmt.Sprintf("avg latency %.0fms", s.AvgLatencyMS)),
	))
	card3 := styleCard.Render(fmt.Sprintf("%s\n%s\n%s",
		styleMuted.Render("CACHE HIT RATE"),
		styleTitle.Render(fmt.Sprintf("%.1f%%", s.CacheHitRatio*100)),
		renderProgressBar(s.CacheHitRatio, 18),
	))
	card4Footer := styleMuted.Render(fmt.Sprintf("shadow waste ~$%.4f", s.TotalShadowSavingsUSD))
	if s.TotalTrimmedTokens > 0 {
		card4Footer = styleOK.Render(fmt.Sprintf("trimmed -%s ($%.4f)", formatTokens(s.TotalTrimmedTokens), s.TotalTrimmedSavingsUSD))
	}
	card4 := styleCard.Render(fmt.Sprintf("%s\n%s\n%s",
		styleMuted.Render("NET CACHE SAVINGS"),
		styleOK.Render(fmt.Sprintf("$%.4f", s.TotalNetCacheSavingsUSD)),
		card4Footer,
	))

	fmt.Println(lipgloss.JoinHorizontal(lipgloss.Top, card1, card2, card3, card4))
	fmt.Println()

	// Token totals
	fmt.Printf("%s  input=%s  cache_read=%s  cache_write=%s  output=%s\n\n",
		styleTitle.Render("TOKENS"),
		formatTokens(s.InputTokens),
		styleOK.Render(formatTokens(s.CacheReadTokens)),
		styleWarn.Render(formatTokens(s.CacheCreationTokens)),
		formatTokens(s.OutputTokens),
	)

	// Model table
	if len(s.ByModel) > 0 {
		fmt.Println(styleTitle.Render("BY MODEL"))
		fmt.Println(styleTableHeader.Render(fmt.Sprintf("%-28s %6s %8s %8s %8s %7s %10s",
			"MODEL", "REQS", "READ", "WRITE", "OUT", "HIT%", "COST")))
		for _, m := range s.ByModel {
			fmt.Printf("%-28s %6d %8s %8s %8s %6.1f%% %10s\n",
				truncate(m.Model, 28),
				m.Requests,
				formatTokens(m.CacheReadTokens),
				formatTokens(m.CacheCreationTokens),
				formatTokens(m.OutputTokens),
				m.CacheHitRatio*100,
				formatUSD(m.CostUSD, m.Estimated),
			)
		}
		fmt.Println()
	}

	// Git Repo & Branch table
	if len(s.ByRepoBranch) > 0 {
		fmt.Println(styleTitle.Render("BY GIT REPO & BRANCH"))
		fmt.Println(styleTableHeader.Render(fmt.Sprintf("%-28s %-20s %6s %9s %7s %10s",
			"REPO", "BRANCH", "REQS", "MAIN/SUB", "HIT%", "COST")))
		for _, r := range s.ByRepoBranch {
			fmt.Printf("%-28s %-20s %6d %4d/%-4d %6.1f%% %10s\n",
				truncate(r.Repo, 28),
				truncate(r.Branch, 20),
				r.Requests,
				r.MainAgentRequests,
				r.SubagentRequests,
				r.CacheHitRatio*100,
				formatUSD(r.CostUSD, r.Estimated),
			)
		}
		fmt.Println()
	}

	// Context hygiene & anomalies summary
	fmt.Println(styleTitle.Render("CONTEXT HEALTH & ANOMALIES"))
	fmt.Printf("  Cache Miss Anomalies:            %s\n", formatAlertCount(s.CacheMissAnomalies))
	fmt.Printf("  Duplicate Read Calls:            %d (%s est. wasted tokens)\n",
		s.DuplicateReadCalls, formatTokens(s.DuplicateReadWastedTokens))
	fmt.Printf("  ANSI Escape Overhead:            %s est. wasted tokens\n", formatTokens(s.ANSIWastedTokens))
	fmt.Printf("  Honest Shadow Savings:           %s\n", styleWarn.Render(fmt.Sprintf("$%.4f", s.TotalShadowSavingsUSD)))
	fmt.Printf("  Actual Trimmed Tokens & Savings: %s (%s)\n",
		styleOK.Render(formatTokens(s.TotalTrimmedTokens)+" tokens"),
		styleOK.Render(fmt.Sprintf("$%.4f USD saved", s.TotalTrimmedSavingsUSD)),
	)
	fmt.Printf("  Post-Cache Tool Surface:         %s (~%s est. tokens after cache_control)\n",
		formatBytes(s.TotalPostCacheToolBytes),
		formatTokens(s.TotalPostCacheToolBytes/4),
	)
	if s.TotalRetries > 0 || s.TotalFailovers > 0 {
		fmt.Printf("  Resilience (Retries / Failover): %d retries · %d failovers\n",
			s.TotalRetries, s.TotalFailovers)
	}
}

func renderInspect(rep *storage.InspectReport) {
	fmt.Println(styleBrand.Render("TRIMBAR INSPECT") + " " + styleMuted.Render(fmt.Sprintf("(%d turns)", len(rep.Turns))))
	fmt.Println()

	if len(rep.Turns) == 0 {
		fmt.Println(styleMuted.Render("No matching requests found."))
		return
	}

	// 1. Context Composition
	fmt.Println(styleTitle.Render("CONTEXT COMPOSITION (EST. TOKENS)"))
	total := rep.Context.TotalEstTokens
	if total <= 0 {
		total = 1
	}
	sections := []struct {
		label  string
		tokens int64
	}{
		{"system", rep.Context.SystemTokens},
		{"tools", rep.Context.ToolsTokens},
		{"tool_results", rep.Context.ToolResultsTokens},
		{"messages", rep.Context.OtherMsgTokens},
	}
	for _, sec := range sections {
		ratio := float64(sec.tokens) / float64(total)
		fmt.Printf("  %-14s %s  %5.1f%%  (%s)\n",
			sec.label,
			renderProgressBar(ratio, 24),
			ratio*100,
			formatTokens(sec.tokens),
		)
	}
	fmt.Println()

	// 2. Recent Turns Table
	fmt.Println(styleTitle.Render("RECENT TURNS"))
	fmt.Println(styleTableHeader.Render(fmt.Sprintf("%-9s %-20s %-16s %-5s %6s %9s %s",
		"TIME", "REPO@BRANCH", "MODEL", "ROLE", "HIT%", "COST", "FLAGS")))
	for _, t := range rep.Turns {
		repoBranch := "-"
		if t.GitRepo != "" {
			repoBranch = t.GitRepo
			if t.GitBranch != "" {
				repoBranch += "@" + t.GitBranch
			}
		}
		var flags []string
		if t.StatusCode >= 400 {
			flags = append(flags, styleErr.Render(fmt.Sprintf("HTTP-%d", t.StatusCode)))
		}
		if t.TrimmedTokens > 0 {
			flags = append(flags, styleOK.Render(fmt.Sprintf("TRIM:-%s", formatTokens(int64(t.TrimmedTokens)))))
		}
		if t.FailoverUsed {
			flags = append(flags, styleWarn.Render("FAILOVER"))
		} else if t.RetryCount > 0 {
			flags = append(flags, styleWarn.Render(fmt.Sprintf("RETRY:%d", t.RetryCount)))
		}
		if t.CacheMissAnomaly {
			flags = append(flags, styleErr.Render("CACHE-MISS"))
		}
		if t.DuplicateReadCount > 0 {
			flags = append(flags, styleWarn.Render(fmt.Sprintf("DUP-READ:%d", t.DuplicateReadCount)))
		}
		if t.ANSISequenceCount > 0 {
			flags = append(flags, styleWarn.Render(fmt.Sprintf("ANSI:%d", t.ANSISequenceCount)))
		}
		roleShort := "main"
		if t.AgentRole == anthropic.AgentRoleSubagent {
			roleShort = "sub"
		}
		fmt.Printf("%-9s %-20s %-16s %-5s %5.1f%% %9s %s\n",
			t.Timestamp.Local().Format("15:04:05"),
			truncate(repoBranch, 20),
			truncate(shortModel(t.Model), 16),
			roleShort,
			t.CacheHitRatio()*100,
			formatUSD(t.CostUSD, t.CostEstimated),
			strings.Join(flags, " "),
		)
	}
	fmt.Println()

	// 3. Duplicate Read Hotspots
	if len(rep.DuplicateHotspots) > 0 {
		fmt.Println(styleTitle.Render("DUPLICATE READ HOTSPOTS"))
		fmt.Println(styleTableHeader.Render(fmt.Sprintf("%-48s %10s %12s", "FILE", "REDUNDANT", "WASTED TOK")))
		for i, h := range rep.DuplicateHotspots {
			if i >= 10 {
				break
			}
			fmt.Printf("%-48s %10d %12s\n",
				truncateLeft(h.FilePath, 48),
				h.RedundantReads,
				formatTokens(int64(h.WastedTokens)),
			)
		}
		fmt.Println()
	}

	// 4. Active Trimming, Post-Cache Surface & ANSI Overhead
	if rep.TotalTrimmedTokens > 0 || rep.TotalPostCacheToolBytes > 0 {
		fmt.Println(styleTitle.Render("POST-CACHE TRIMMING SURFACE"))
		fmt.Printf("  Post-cache tool_result bytes: %s (~%s est. tokens after cache_control)\n",
			formatBytes(rep.TotalPostCacheToolBytes),
			formatTokens(rep.TotalPostCacheToolBytes/4),
		)
		if rep.TotalTrimmedTokens > 0 {
			fmt.Printf("  Actively trimmed:             %s tokens (%s actual USD saved)\n",
				styleOK.Render(formatTokens(rep.TotalTrimmedTokens)),
				styleOK.Render(fmt.Sprintf("$%.4f", rep.TotalTrimmedSavingsUSD)),
			)
		}
		fmt.Println()
	}
	if rep.TotalANSISequences > 0 {
		fmt.Println(styleTitle.Render("ANSI ESCAPE POLLUTION"))
		fmt.Printf("  %d escape sequences across inspected tool_results (~%s wasted tokens)\n\n",
			rep.TotalANSISequences, formatTokens(rep.TotalANSIWasted))
	}

	// 5. Cache Miss Anomalies
	if len(rep.CacheMissAnomalies) > 0 {
		fmt.Println(styleErr.Render("CACHE MISS ANOMALIES"))
		for _, a := range rep.CacheMissAnomalies {
			fmt.Printf("  %s  [%s]  %s\n",
				a.Timestamp.Local().Format("2006-01-02 15:04:05"),
				truncate(a.SessionID, 14),
				a.CacheMissReason,
			)
		}
	}
}

func renderProgressBar(ratio float64, width int) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio*float64(width) + 0.5)
	if filled > width {
		filled = width
	}
	bar := styleOK.Render(strings.Repeat("█", filled)) +
		styleMuted.Render(strings.Repeat("░", width-filled))
	return bar
}

func persistRequestLog(repo *storage.Repository, rec *storage.RequestLog, errOut io.Writer) {
	if repo == nil || rec == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := repo.InsertRequest(ctx, rec); err != nil && errOut != nil {
		fmt.Fprintf(errOut, "%s failed to persist request to SQLite (user response unaffected): %v\n",
			styleWarn.Render("WARN"), err)
	}
}

// isLoopbackHost reports whether host binds strictly to a local loopback interface.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(strings.Trim(host, "[]")))
	if h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// nonLoopbackSecurityWarning returns a prominent warning when Trimbar is exposed
// on a non-loopback interface (e.g. 0.0.0.0) over plaintext HTTP.
func nonLoopbackSecurityWarning(host string) string {
	if isLoopbackHost(host) {
		return ""
	}
	warnBox := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("196")).
		Padding(0, 2)
	msg := strings.Join([]string{
		styleErr.Render("SECURITY WARNING: Non-loopback bind address (--host " + host + ")"),
		"Trimbar is listening on a network-accessible interface over plaintext HTTP and",
		"accepts sensitive Authorization / x-api-key headers plus /ui & /api/* telemetry.",
		"Use --host 127.0.0.1 unless protected by TLS and strict firewall/VPN rules.",
	}, "\n")
	return warnBox.Render(msg)
}

func formatAlertCount(n int64) string {
	if n > 0 {
		return styleErr.Render(fmt.Sprintf("%d", n))
	}
	return styleOK.Render("0")
}

func formatTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000.0)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000.0)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatBytes(n int64) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.2f MB", float64(n)/(1024.0*1024.0))
	case n >= 1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024.0)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func shortModel(model string) string {
	m := strings.TrimPrefix(model, "claude-")
	// Trim trailing YYYYMMDD snapshot suffix for compact display.
	if len(m) > 9 && m[len(m)-9] == '-' {
		if _, err := strconv.Atoi(m[len(m)-8:]); err == nil {
			m = m[:len(m)-9]
		}
	}
	return m
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen || maxLen <= 3 {
		return s
	}
	return s[:maxLen-1] + "…"
}

func truncateLeft(s string, maxLen int) string {
	if len(s) <= maxLen || maxLen <= 3 {
		return s
	}
	return "…" + s[len(s)-(maxLen-1):]
}

func parseSince(s string) (time.Time, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" || v == "all" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(v, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil || days <= 0 {
			return time.Time{}, fmt.Errorf("invalid --since value %q", s)
		}
		return time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour), nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid --since duration %q (examples: 24h, 7d, all)", s)
	}
	return time.Now().UTC().Add(-d), nil
}
