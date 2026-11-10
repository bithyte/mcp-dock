// Command fakeupstream is a stdio MCP server used by tests.
//
// Tools: echo, sleep, crash, getenv, cwd, notify, add_tool (registers "late").
// Env:
//
//	FAKE_CANCEL_FILE  sleep writes "cancelled" here when its context is cancelled
//	FAKE_LONG_TOOL=1  adds a tool whose name is 70 chars long
//	FAKE_FAIL_START   exit 1 immediately while the named file exists
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// longToolName is 70 characters, too long once prefixed by the gateway.
var longToolName = "long_" + strings.Repeat("x", 65)

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

type echoIn struct {
	Text string `json:"text"`
}

type sleepIn struct {
	MS int `json:"ms"`
}

type getenvIn struct {
	Name string `json:"name"`
}

type attachment struct {
	Path string `json:"path"`
	Text string `json:"text,omitempty"`
}

type notifyIn struct {
	Text       string      `json:"text"`
	Attachment *attachment `json:"attachment,omitempty"`
}

type empty struct{}

func main() {
	if f := os.Getenv("FAKE_FAIL_START"); f != "" {
		if _, err := os.Stat(f); err == nil {
			fmt.Fprintln(os.Stderr, "fail start: file present")
			os.Exit(1)
		}
	}
	fmt.Fprintln(os.Stderr, "hello stderr")

	s := mcp.NewServer(&mcp.Implementation{Name: "fakeupstream", Version: "0.0.1"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "returns text"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, any, error) {
			return text(in.Text), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "sleep", Description: "sleeps ms, sending progress every 50ms"},
		func(ctx context.Context, req *mcp.CallToolRequest, in sleepIn) (*mcp.CallToolResult, any, error) {
			deadline := time.After(time.Duration(in.MS) * time.Millisecond)
			tick := time.NewTicker(50 * time.Millisecond)
			defer tick.Stop()
			token := req.Params.GetProgressToken()
			n := 0
			for {
				select {
				case <-deadline:
					return text("done"), nil, nil
				case <-ctx.Done():
					if f := os.Getenv("FAKE_CANCEL_FILE"); f != "" {
						_ = os.WriteFile(f, []byte("cancelled"), 0o600)
					}
					return nil, nil, ctx.Err()
				case <-tick.C:
					n++
					if token != nil {
						_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
							ProgressToken: token,
							Progress:      float64(n),
						})
					}
				}
			}
		})

	mcp.AddTool(s, &mcp.Tool{Name: "crash", Description: "exits the process"},
		func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, any, error) {
			os.Exit(1)
			return nil, nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "getenv", Description: "returns an env var or <unset>"},
		func(_ context.Context, _ *mcp.CallToolRequest, in getenvIn) (*mcp.CallToolResult, any, error) {
			v, ok := os.LookupEnv(in.Name)
			if !ok {
				v = "<unset>"
			}
			return text(v), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "cwd", Description: "returns the working dir"},
		func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, any, error) {
			wd, err := os.Getwd()
			if err != nil {
				return nil, nil, err
			}
			return text(wd), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "notify", Description: "jaypost-shaped notify"},
		func(context.Context, *mcp.CallToolRequest, notifyIn) (*mcp.CallToolResult, any, error) {
			return text("ok"), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "add_tool", Description: "registers tool late"},
		func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, any, error) {
			mcp.AddTool(s, &mcp.Tool{Name: "late", Description: "added at runtime"},
				func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, any, error) {
					return text("late"), nil, nil
				})
			return text("added"), nil, nil
		})

	if os.Getenv("FAKE_LONG_TOOL") == "1" {
		mcp.AddTool(s, &mcp.Tool{Name: longToolName, Description: "name is 70 chars"},
			func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, any, error) {
				return text("long"), nil, nil
			})
	}

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "server stopped:", err)
		os.Exit(1)
	}
}
