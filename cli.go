package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() { os.Exit(run(os.Args[1:])) }

// Keep exit decisions outside resource-owning code, so terminal restoration,
// session and HTTP cleanup run on errors and signals too.
func run(args []string) (status int) {
	defer func() {
		if r := recover(); r != nil {
			infof("Panic: %v\n", r)
			status = 1
		}
	}()
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	prompt := fs.String("prompt", "", "User prompt")
	once := fs.Bool("once", false, "Process -prompt once and exit")
	pipe := fs.Bool("pipe", false, "Read stdin as data; combine with -prompt; implies -once")
	load := fs.String("load", "", "Load a session without updating its file")
	save := fs.String("save", "", "Save a snapshot to a new file on exit")
	resume := fs.String("continue", "", "Load if present and checkpoint to the same file")
	fs.BoolVar(&verbose, "verbose", false, "Show reasoning, tool arguments and tool results on stderr")
	fs.StringVar(&apiBase, "api-base", envDefault("AGENT_API_BASE", "http://localhost:8080/v1"), "API base URL; default from AGENT_API_BASE")
	fs.StringVar(&apiKey, "api-key", os.Getenv("AGENT_API_KEY"), "API key; default from AGENT_API_KEY")
	fs.Lookup("api-key").DefValue = "" // keep the effective default out of help
	fs.StringVar(&modelName, "model", os.Getenv("AGENT_MODEL"), "Model name; default from AGENT_MODEL")
	fs.StringVar(&reasoningField, "reasoning-field", "reasoning_content", "Reasoning delta field")
	fs.StringVar(&contentField, "content-field", "content", "Content delta field")
	fs.StringVar(&toolCallsField, "tool-calls-field", "tool_calls", "Tool-call delta field")
	fs.StringVar(&finishReasonField, "finish-reason-field", "finish_reason", "Choice finish field")
	fs.IntVar(&httpMaxIdleConns, "http-max-idle-conns", 100, "Maximum retained idle HTTP connections")
	fs.DurationVar(&httpIdleTimeout, "http-idle-timeout", 5*time.Minute, "Idle HTTP connection lifetime")
	fs.DurationVar(&streamIdleTimeout, "stream-idle-timeout", 5*time.Minute, "Network inactivity timeout; 0 disables it")
	fs.DurationVar(&streamIdleTimeout, "tool-call-idle-timeout", 5*time.Minute, "Alias for -stream-idle-timeout")
	fs.Func("extra", "Base64 JSON object merged into the API request; repeatable", func(s string) error {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return fmt.Errorf("invalid base64 for -extra: %w", err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
			return errors.New("-extra must decode to a JSON object")
		}
		extraRequestFields = append(extraRequestFields, obj)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	apiBase = strings.TrimRight(apiBase, "/")
	u, err := url.Parse(apiBase)
	if modelName == "" || fs.NArg() != 0 || (*once && *prompt == "" && !*pipe) {
		infof("Error: specify a model and provide -prompt with -once (or use -pipe)\n")
		return 1
	}
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		infof("Error: -api-base must be an HTTP(S) base URL without credentials, query or fragment\n")
		return 1
	}
	if streamIdleTimeout < 0 || httpIdleTimeout < 0 || httpMaxIdleConns < 0 {
		infof("Error: timeouts and idle connection count must be nonnegative\n")
		return 1
	}
	if *load != "" && *resume != "" {
		infof("Error: -load and -continue are mutually exclusive\n")
		return 1
	}
	if sameSessionPath(*save, *resume) {
		infof("Save failed: snapshot must use a different file from -continue\n")
		return 1
	}
	if *save != "" {
		if _, err := os.Lstat(*save); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				err = errors.New("path already exists")
			}
			infof("Save failed: snapshot requires a new file: %v\n", err)
			return 1
		}
	}
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	stdoutWriter = newOutputWriter(ctx, os.Stdout)
	stderrWriter = newOutputWriter(ctx, os.Stderr)
	var signalStatus atomic.Int32
	autonomous := *once || *pipe
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig == os.Interrupt {
					if cancel := interruptMgr.take(); cancel != nil && !autonomous {
						cancel()
						continue
					}
					signalStatus.Store(130)
				} else {
					signalStatus.Store(143)
				}
				shutdown()
			case <-ctx.Done():
				return
			}
		}
	}()
	failureStatus := func(err error) int {
		if code := signalStatus.Load(); code != 0 {
			return int(code)
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		if err != nil {
			return 1
		}
		return 0
	}
	sess := newSession()
	if *resume != "" {
		sess, err = continueSession(*resume)
	} else if *load != "" {
		sess, err = loadSession(*load)
	}
	if err != nil || sess == nil {
		infof("Error opening session: %v (session must be nonempty)\n", err)
		return 1
	}
	defer func() {
		if err := sess.release(); err != nil {
			infof("Session cleanup failed: %v\n", err)
			if status == 0 {
				status = 1
			}
		}
	}()
	if *pipe {
		p, err := readInput(ctx, func() (string, error) { return pipePrompt(*prompt, os.Stdin) })
		if err != nil {
			infof("Error reading stdin: %v\n", err)
			return failureStatus(err)
		}
		if p == "" {
			infof("Error: -pipe requires stdin data or -prompt\n")
			return 1
		}
		*prompt, *once = p, true
	}
	if *save != "" {
		defer func() {
			if err := saveSession(*save, sess); err != nil {
				infof("Save failed: %v\n", err)
				if status == 0 {
					status = 1
				}
			}
		}()
	}
	httpClient = newHTTPClient()
	defer httpClient.CloseIdleConnections()
	// Autonomous runs never alter terminal state or put prompts on stdout.
	if !*once {
		out, _ := os.Stdout.Stat()
		if out != nil && out.Mode()&os.ModeCharDevice != 0 {
			if old, err := makeRaw(int(os.Stdin.Fd())); err == nil {
				termRaw, termFd, termOld = true, int(os.Stdin.Fd()), old
				defer stopConsole(termFd, termOld)
				put(pasteStart)
			}
		}
	}
	infof("%s\n", sess.persistenceNotice())
	showSystemPrompt(sess.Transcript)
	turn := func(msg Message) error {
		before := len(sess.Transcript)
		beforeCount := sess.messageCount()
		sess.Transcript = append(sess.Transcript, msg)
		infCtx, cancel := context.WithCancel(ctx)
		interruptMgr.set(cancel)
		err := runInferenceLoop(infCtx, sess)
		cancel()
		interruptMgr.clear()
		if err != nil && sess.messageCount() == beforeCount+1 {
			sess.Transcript[before] = Message{} // release the rejected input's backing-slot reference
			sess.Transcript = sess.Transcript[:before]
		} else {
			clearPending()
		}
		if sess.saveFile != "" && !errors.Is(err, errCheckpoint) {
			if saveErr := checkpointSession(sess.saveFile, sess); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("%w: %w", errCheckpoint, saveErr))
			}
		}
		if err != nil {
			infof("Runtime error: %v\n", err)
		}
		return err
	}
	if *prompt != "" {
		err := turn(Message{Role: "user", Content: *prompt})
		status = failureStatus(err)
		if *once || ctx.Err() != nil || errors.Is(err, errCheckpoint) {
			return status
		}
	}
	reader := bufio.NewReader(os.Stdin)
	for {
		if termRaw {
			put("\r> ")
		}
		if err := outputFailure(); err != nil {
			infof("Output error: %v\n", err)
			return 1
		}
		input, err := readInput(ctx, func() (string, error) { return readLine(reader) })
		if err != nil && !errors.Is(err, io.EOF) {
			infof("Input error: %v\n", err)
			return failureStatus(err)
		}
		command := strings.TrimSpace(input)
		literal := strings.ContainsAny(input, "\r\n")
		if termRaw && command == "/paste" && !literal {
			put("Paste text; Ctrl+D sends.\n")
			input, err = readInput(ctx, func() (string, error) { return readInputLine(reader, true) })
			if err != nil {
				infof("Input error: %v\n", err)
				return failureStatus(err)
			}
			command, literal = strings.TrimSpace(input), true
		}
		if command == "" {
			if err == io.EOF {
				return status
			}
			continue
		}
		if !literal && (command == "exit" || command == "quit") {
			return status
		}
		handled := false
		var commandErr error
		if !literal {
			handled, commandErr = handleCommand(command, &sess)
		}
		if commandErr != nil {
			infof("Save failed: %v\n", commandErr)
			return failureStatus(commandErr)
		}
		if handled {
			continue
		}
		turnErr := turn(buildUserMessage(input, &pending))
		status = failureStatus(turnErr)
		if errors.Is(turnErr, errCheckpoint) {
			return status
		}
		if ctx.Err() != nil {
			return failureStatus(ctx.Err())
		}
	}
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// One outstanding stdin read at most. On shutdown the process returns through
// its defers and exits; stdin stays open until terminal restoration completes.
func readInput(ctx context.Context, read func() (string, error)) (string, error) {
	type result struct {
		text string
		err  error
	}
	ch := make(chan result, 1)
	go func() { text, err := read(); ch <- result{text, err} }()
	select {
	case r := <-ch:
		return r.text, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func handleCommand(input string, sess **Session) (bool, error) {
	cmd, arg, _ := strings.Cut(input, " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "/help":
		printHelp()
	case "/pending":
		showPending()
	case "/clear":
		clearPending()
		put("Pending media cleared.\n")
	case "/memory":
		showMemory(*sess)
	case "/image", "/image-url":
		if arg == "" {
			putf("Usage: %s <path or URL>\n", cmd)
			break
		}
		uri := arg
		if cmd == "/image" {
			var err error
			uri, _, err = readImage(arg)
			if err != nil {
				putf("Image error: %v\n", err)
				break
			}
		}
		pending.images = append(pending.images, ContentBlock{Type: "image_url", ImageURL: &ImageURL{URL: uri, Detail: "auto"}})
		putf("Image appended (%d pending).\n", len(pending.images))
	case "/save", "/load", "/reload", "/continue":
		if arg == "" {
			putf("Usage: %s <filename>\n", cmd)
			break
		}
		if cmd == "/save" {
			if err := saveSession(arg, *sess); err != nil {
				return true, fmt.Errorf("%w: %w", errCheckpoint, err)
			}
			put("Session snapshot saved.\n")
			infof("%s\n", (*sess).persistenceNotice())
			break
		}
		old := *sess
		var loaded *Session
		var err error
		if cmd == "/continue" && !old.owner.matches(arg) {
			loaded, err = continueSession(arg)
		} else {
			loaded, err = loadReservedSession(arg, old.owner)
		}
		if err != nil || loaded == nil {
			if cmd == "/continue" {
				return true, fmt.Errorf("%w: cannot continue session: %v", errCheckpoint, err)
			}
			putf("Session unchanged: %v (session must be nonempty)\n", err)
			break
		}
		if cmd == "/continue" && old.owner.matches(arg) {
			loaded.owner, loaded.ownsOwner = old.owner, old.ownsOwner
			loaded.saveFile = old.saveFile
			old.owner, old.ownsOwner = nil, false
		}
		if err := old.release(); err != nil {
			loaded.close()
			return true, fmt.Errorf("%w: %w", errCheckpoint, err)
		}
		*sess = loaded
		clearPending()
		showSystemPrompt(loaded.Transcript)
		put("Session opened.\n")
		infof("%s\n", loaded.persistenceNotice())
	default:
		return false, nil
	}
	return true, nil
}
