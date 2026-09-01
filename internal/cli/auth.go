package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/nxrmqlly/jittrippin/internal/config"
	"github.com/urfave/cli/v3"
)

const loginTimeout = 5 * time.Minute

func CmdAuth() *cli.Command {
	return &cli.Command{
		Name:  "auth",
		Usage: "manage authentication to a jittrippin daemon",
		Commands: []*cli.Command{
			CmdAuthLogin(),
			CmdAuthLogout(),
			CmdAuthStatus(),
		},
	}
}

func CmdAuthLogin() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "log in to a jittrippin daemon",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "provider", Usage: "OAuth provider (default: first advertised by the daemon)"},
			&cli.StringFlag{Name: "daemon", Usage: "daemon base URL (default: from user config)"},
			&cli.BoolFlag{Name: "no-open", Usage: "print the URL instead of opening a browser"},
		},
		Action: handleAuthLogin,
	}
}

func ensureSession(c *cli.Command) (*daemonClient, error) {
	sess, err := loadSession()
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.Token == "" {
		return nil, errors.New("not logged in: run 'jt auth login' first")
	}
	base, err := ensureDaemonURL(c)
	if err != nil {
		return nil, err
	}
	client := newDaemonClient(base)
	client.token = sess.Token
	return client, nil
}

func ensureDaemonURL(c *cli.Command) (string, error) {
	if u := c.String("daemon"); u != "" {
		return strings.TrimRight(u, "/"), nil
	}
	cfg, err := config.LoadUserConfig()
	if err != nil {
		return "", err
	}
	if config.UserConfigExists() {
		return cfg.Daemon, nil
	}
	u := config.DefaultDaemon
	if err := huh.NewInput().Title("Daemon URL").Value(&u).Run(); err != nil {
		return "", err
	}
	cfg.Daemon = strings.TrimRight(u, "/")
	if err := config.SaveUserConfig(cfg); err != nil {
		return "", err
	}
	return cfg.Daemon, nil
}

func checkHealth(_ context.Context, c *daemonClient) error {
	if err := c.do(http.MethodGet, "/api/v1/health", nil, nil); err != nil {
		return fmt.Errorf("daemon not reachable: %w", err)
	}
	return nil
}

func oauthDance(ctx context.Context, client *daemonClient, provider string, noOpen bool) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()

	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	url, err := client.Begin(provider, redirect)
	if err != nil {
		return "", err
	}
	fmt.Printf("Open this URL in your browser:\n\n	%s\n\n", url)
	// TODO: fix browser not opening properly on platforms
	if !noOpen && false {
		_ = openBrowser(url)
	}
	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("auth_code")
		if code == "" {
			http.Error(w, "missing auth_code", http.StatusBadRequest)
			return
		}
		select {
		case codeCh <- code:
		default:
		}
		// todo: change this to html perhaps, or not...
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("You can close this tab and return to the terminal."))
	})}
	go func() { errCh <- srv.Serve(ln) }()
	var authCode string
	if err := RunWithProgress("Waiting for browser callback", func() error {
		select {
		case authCode = <-codeCh:
		case err := <-errCh:
			return err
		case <-time.After(loginTimeout):
			return errors.New("timed out: login not completed in time")
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}); err != nil {
		return "", err
	}

	_ = srv.Close()
	return client.Exchange(authCode)
}

func handleAuthLogin(ctx context.Context, c *cli.Command) error {
	base, err := ensureDaemonURL(c)
	if err != nil {
		return err
	}
	client := newDaemonClient(base)
	if err := checkHealth(ctx, client); err != nil {
		return err
	}
	provider := c.String("provider")
	if provider == "" {
		providers, err := client.Providers()
		if err != nil {
			return err
		}
		if len(providers) == 0 {
			return errors.New("daemon advertises no OAuth Providers")
		}
		if err := huh.NewSelect[string]().Title("OAuth provider").
			Options(huh.NewOptions(providers...)...).
			Value(&provider).Run(); err != nil {
			return err
		}

	}
	token, err := oauthDance(ctx, client, provider, c.Bool("no-open"))
	if err != nil {
		return err
	}
	if err := saveSession(&Session{Token: token, Provider: provider}); err != nil {
		return err
	}
	fmt.Printf("%s Logged in successfully.\n", successIcon())
	return nil
}

func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

func CmdAuthLogout() *cli.Command {
	return &cli.Command{
		Name:   "logout",
		Usage:  "log out of the current jittrippin daemon",
		Action: handleAuthLogout,
	}
}

func handleAuthLogout(ctx context.Context, c *cli.Command) error {
	if !hasSession() {
		fmt.Println("Not logged in.")
		return nil
	}
	sess, _ := loadSession()
	base, err := ensureDaemonURL(c)
	if err == nil {
		client := newDaemonClient(base)
		client.token = sess.Token
		// best effort logout, token may be already invalid
		_ = client.Logout()
	}
	if err := clearSession(); err != nil {
		return err
	}
	fmt.Println("Logged out.")
	return nil
}

func CmdAuthStatus() *cli.Command {
	return &cli.Command{
		Name:   "status",
		Usage:  "show login status and linked integrations",
		Flags:  []cli.Flag{&cli.StringFlag{Name: "daemon", Usage: "daemon base URL (default: from user config)"}},
		Action: handleAuthStatus,
	}
}

func handleAuthStatus(ctx context.Context, c *cli.Command) error {
	base, err := ensureDaemonURL(c)
	if err != nil {
		return err
	}
	fmt.Printf("Daemon:    %s\n", base)

	sess, err := loadSession()
	if err != nil {
		return err
	}
	if sess == nil || sess.Token == "" {
		fmt.Println("Logged in: no, run `jt auth login`")
		return nil
	}

	client := newDaemonClient(base)
	client.token = sess.Token
	if err := checkHealth(ctx, client); err != nil {
		return err
	}
	me, err := client.Me()
	if err != nil {
		return err
	}
	fmt.Printf("User:      %s (%s)\n", me.User.Name, me.User.ID)
	if len(me.Identities) > 0 {
		fmt.Println("Linked accounts:")
		for _, id := range me.Identities {
			fmt.Printf("  - %s", id.Provider)
			if id.Login != "" {
				fmt.Printf(" (%s)", id.Login)
			}
			fmt.Println()
		}
	}
	integrations, err := client.MyIntegrations()
	if err != nil {
		return err
	}
	if len(integrations) > 0 {
		fmt.Println("Integrations:")
		for _, in := range integrations {
			fmt.Printf("  - %s", in.Provider)
			if len(in.Installations) > 0 {
				logins := make([]string, 0, len(in.Installations))
				for _, a := range in.Installations {
					logins = append(logins, a.AccountLogin)
				}
				fmt.Printf(" (installed on: %s)", strings.Join(logins, ", "))
			}
			fmt.Println()
		}
	}
	return nil
}
