// Handshake-only live diagnostic: owns an ephemeral outpost and fresh cloud
// sessions, never sends user_message, uploads history or invokes any tool.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kilbertert/quick2api/internal/chat"
	"github.com/kilbertert/quick2api/internal/config"
	"github.com/kilbertert/quick2api/internal/session"
)

type result struct {
	Index            int      `json:"index"`
	Mode             string   `json:"mode"`
	Stages           []string `json:"stages"`
	FrameTypes       []string `json:"frameTypes"`
	MS               int64    `json:"ms"`
	Connected        bool     `json:"connected"`
	Confirmed        bool     `json:"confirmed"`
	AliveAfterSettle bool     `json:"aliveAfterSettle"`
	ErrorKind        string   `json:"errorKind,omitempty"`
	CloseCode        int      `json:"closeCode,omitempty"`
	UpstreamCode     string   `json:"upstreamCode,omitempty"`
}

var frameName = regexp.MustCompile(`^[a-z_]{1,80}$`)
var upstreamCode = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "candidate configuration (credentials never printed)")
	outPath := flag.String("out", "", "new private report file")
	flag.Parse()
	if *cfgPath == "" || *outPath == "" {
		return errors.New("required: -config and -out")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return errors.New("unable to load private diagnostic configuration")
	}
	// Session/outpost code may log account metadata; retain only this report.
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o, err := session.Spawn(ctx.Done(), session.SpawnOpts{AgentPath: cfg.Quick.AgentPath, Manifest: cfg.Quick.Manifest, BuildStage: cfg.Quick.BuildStage, ProfileRoot: cfg.Quick.ProfileRoot, Port: 0})
	if err != nil {
		return errors.New("unable to start diagnostic-owned ephemeral outpost")
	}
	defer o.Close()
	fmt.Printf("{\"diagnosticPid\":%d,\"outpostURL\":%q,\"userMessages\":0}\n", os.Getpid(), o.URL)
	sess, err := o.FetchSession()
	if err != nil || sess.OutpostID == "" {
		return errors.New("diagnostic session or registered device unavailable")
	}
	c := &chat.Client{SurfaceType: cfg.Quick.SurfaceType, Channel: cfg.Quick.Channel, Origin: cfg.Quick.Origin, HandshakeTimeout: cfg.HandshakeTimeout(), StrictHandshake: cfg.Quick.StrictHandshake, Proxy: cfg.Quick.Proxy}
	results := make([]result, 12)
	probe := func(index int, mode string) {
		started := time.Now()
		r := result{Index: index, Mode: mode, Stages: []string{}, FrameTypes: []string{}}
		probeCtx, stop := context.WithTimeout(ctx, 12*time.Second)
		defer stop()
		cn, err := c.NewNativeConversation(probeCtx, sess, chat.TurnOpts{OnEvent: func(ev chat.TurnEvent) {
			if ev.Kind == "stage" {
				r.Stages = append(r.Stages, ev.Text)
			}
			if ev.Kind == "frame" && ev.Frame != nil && len(r.FrameTypes) < 64 {
				if frameName.MatchString(ev.Frame.Type) {
					r.FrameTypes = append(r.FrameTypes, ev.Frame.Type)
				}
			}
		}})
		if err == nil {
			r.Connected = true
			select {
			case <-probeCtx.Done():
			case <-time.After(100 * time.Millisecond):
			}
			r.AliveAfterSettle = cn.Alive()
			cn.Close()
		} else {
			r.ErrorKind = "handshake_failure"
			var closeErr *websocket.CloseError
			var ue *chat.UpstreamError
			if errors.As(err, &closeErr) {
				r.CloseCode = closeErr.Code
			}
			if errors.As(err, &ue) && upstreamCode.MatchString(ue.Code) {
				r.UpstreamCode = ue.Code
			}
			if errors.Is(err, context.Canceled) {
				r.ErrorKind = "cancelled"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				r.ErrorKind = "deadline"
			}
		}
		r.MS = time.Since(started).Milliseconds()
		r.Confirmed = slices.Contains(r.Stages, "session_connected") && slices.Contains(r.Stages, "session_created")
		results[index] = r
		encoded, _ := json.Marshal(r)
		fmt.Println(string(encoded))
	}
	for i := 0; i < 6; i++ {
		probe(i, "serial")
	}
	for i := 6; i < 12; i += 2 {
		var wg sync.WaitGroup
		for j := i; j < i+2; j++ {
			wg.Add(1)
			go func(index int) { defer wg.Done(); probe(index, "pair") }(j)
		}
		wg.Wait()
	}
	report := struct {
		SetupPolicy     string   `json:"setupPolicy"`
		StrictHandshake bool     `json:"strictHandshake"`
		UserMessages    int      `json:"userMessages"`
		ToolInvocations int      `json:"toolInvocations"`
		Results         []result `json:"results"`
	}{SetupPolicy: "per-process-identity-serialized", StrictHandshake: cfg.Quick.StrictHandshake, Results: results}
	file, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("unable to create new diagnostic report")
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(report); err != nil {
		return errors.New("unable to save diagnostic report")
	}
	for _, r := range results {
		if !r.Connected || !r.Confirmed || !r.AliveAfterSettle {
			return errors.New("one or more handshake-only probes failed; private report saved")
		}
	}
	return nil
}
