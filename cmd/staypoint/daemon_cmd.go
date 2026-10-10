package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/ipc"
	"github.com/VinnyVanGogh/staypoint/internal/server"
	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Query or control the staypointd background daemon via IPC socket",
}

var daemonPingCmd = &cobra.Command{
	Use:   "ping",
	Short: "Ping staypointd and verify the IPC socket is reachable",
	Run: func(cmd *cobra.Command, args []string) {
		socketPath, _ := cmd.Flags().GetString("socket")
		if socketPath == "" {
			socketPath = ipc.SocketPath()
		}

		conn, err := ipc.Dial(socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

		req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			fmt.Fprintf(os.Stderr, "write error: %v\n", err)
			os.Exit(1)
		}

		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			fmt.Fprintf(os.Stderr, "read error: %v\n", err)
			os.Exit(1)
		}

		var resp map[string]any
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			fmt.Fprintf(os.Stderr, "parse error: %v\n", err)
			os.Exit(1)
		}
		if resp["error"] != nil {
			fmt.Fprintf(os.Stderr, "daemon error: %v\n", resp["error"])
			os.Exit(1)
		}
		fmt.Printf("pong (socket: %s)\n", socketPath)
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Query real-time status from staypointd over IPC",
	Run: func(cmd *cobra.Command, args []string) {
		socketPath, _ := cmd.Flags().GetString("socket")
		if socketPath == "" {
			socketPath = ipc.SocketPath()
		}

		conn, err := ipc.Dial(socketPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

		// Send MCP initialize handshake then tools/call staypoint_status.
		msgs := []map[string]any{
			{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
				"protocolVersion": "2024-11-05",
				"clientInfo":      map[string]any{"name": "staypoint-cli", "version": version},
			}},
			{"jsonrpc": "2.0", "id": nil, "method": "notifications/initialized"},
			{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
				"name": "staypoint_status", "arguments": map[string]any{},
			}},
		}

		enc := json.NewEncoder(conn)
		for _, m := range msgs {
			if err := enc.Encode(m); err != nil {
				fmt.Fprintf(os.Stderr, "write error: %v\n", err)
				os.Exit(1)
			}
		}

		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			var resp map[string]any
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				continue
			}
			// Skip initialize response (id=1) and notifications.
			id, _ := resp["id"].(float64)
			if id != 2 {
				continue
			}
			if resp["error"] != nil {
				fmt.Fprintf(os.Stderr, "daemon error: %v\n", resp["error"])
				os.Exit(1)
			}
			result, _ := resp["result"].(map[string]any)
			content, _ := result["content"].([]any)
			if len(content) > 0 {
				item, _ := content[0].(map[string]any)
				text, _ := item["text"].(string)
				fmt.Println(text)
			}
			return
		}
	},
}

var daemonSocketPathCmd = &cobra.Command{
	Use:   "socket-path",
	Short: "Print the IPC socket path staypointd will use on this host",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(ipc.SocketPath())
	},
}

var daemonServeCmd = &cobra.Command{
	Use:     "serve",
	Aliases: []string{"http"},
	Short:   "Start the local HTTP and SSE daemon server (127.0.0.1 only)",
	Run: func(cmd *cobra.Command, args []string) {
		port, _ := cmd.Flags().GetInt("port")
		token, _ := cmd.Flags().GetString("token")

		if cfg == nil {
			var err error
			cfg, err = config.LoadConfig()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error loading config: %v\n", err)
				os.Exit(1)
			}
		}
		setGateHosts(cfg)

		dbStore, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error opening db: %v\n", err)
			os.Exit(1)
		}
		defer dbStore.Close()

		tokenPath := filepath.Join(cfg.DataDir, "auth_token")
		boardTokenPath := filepath.Join(cfg.DataDir, "board_token")
		srv, err := server.New(server.Options{
			BindHost:       "127.0.0.1",
			Port:           port,
			AuthToken:      token,
			TokenPath:      tokenPath,
			BoardTokenPath: boardTokenPath,
			DB:             dbStore.DB(),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating server: %v\n", err)
			os.Exit(1)
		}

		if err := srv.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "error starting server: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("StayPoint local HTTP and SSE server running\n")
		fmt.Printf("  URL:        %s\n", srv.URL())
		fmt.Printf("  Auth Token: %s\n", srv.Token())
		fmt.Printf("  Token File: %s\n", tokenPath)
		// Board URL uses a one-time nonce (not the long-lived board_token).
		// Only print to a TTY so the URL is not captured in logs or piped output
		// that an agent process could read.
		fi, _ := os.Stdout.Stat()
		if fi != nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			boardURL := fmt.Sprintf("%s/?token=%s&board_nonce=%s", srv.URL(), srv.Token(), srv.BoardNonce())
			fmt.Printf("  Board URL:  %s\n", boardURL)
		}
		fmt.Printf("Press Ctrl+C to stop.\n")

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		fmt.Println("\nShutting down server...")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		fmt.Println("Server stopped.")
	},
}

func init() {
	daemonPingCmd.Flags().String("socket", "", "Override IPC socket path")
	daemonStatusCmd.Flags().String("socket", "", "Override IPC socket path")
	daemonServeCmd.Flags().Int("port", 41421, "Port for the HTTP and SSE server (127.0.0.1 only)")
	daemonServeCmd.Flags().String("token", "", "Override auth token (defaults to saved or generated token)")

	daemonCmd.AddCommand(daemonPingCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonSocketPathCmd)
	daemonCmd.AddCommand(daemonServeCmd)
	rootCmd.AddCommand(daemonCmd)
}

