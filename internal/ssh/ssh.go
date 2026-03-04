package ssh

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"
)

// ClientConfig holds parameters for creating an SSH client
type ClientConfig struct {
	Host    string // IP or hostname
	Port    int    // SSH port (default: 22)
	User    string // SSH username
	KeyFile string // path to private key file
}

// Client wraps an SSH connection and provides command execution methods
type Client struct {
	config ClientConfig
	conn   *ssh.Client
	logger zerolog.Logger
}

// NewClient creates a new SSH client and establishes the connection.
// The caller must call Close() when done.
func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}

	keyBytes, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read SSH key %s: %w", cfg.KeyFile, err)
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse SSH key %s: %w", cfg.KeyFile, err)
	}

	sshConfig := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // matches bash -o StrictHostKeyChecking=no
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	conn, err := ssh.Dial("tcp", addr, sshConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	return &Client{
		config: cfg,
		conn:   conn,
		logger: log.With().Str("ssh_host", cfg.Host).Logger(),
	}, nil
}

// Close closes the SSH connection
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Run executes a command on the remote host and returns stdout output.
// Returns an error if the command exits with a non-zero status.
func (c *Client) Run(cmd string) (string, error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	c.logger.Debug().Str("cmd", cmd).Msg("SSH exec")

	output, err := session.CombinedOutput(cmd)
	result := strings.TrimSpace(string(output))
	if err != nil {
		return result, fmt.Errorf("SSH command failed: %w (output: %s)", err, result)
	}
	return result, nil
}

// RunStreaming executes a command and streams stdout/stderr to the provided writers.
// Blocks until the command completes.
func (c *Client) RunStreaming(cmd string, stdout, stderr *os.File) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	session.Stdout = stdout
	session.Stderr = stderr

	c.logger.Debug().Str("cmd", cmd).Msg("SSH exec streaming")

	return session.Run(cmd)
}

// RunBackground starts a command that persists after the SSH session closes.
// Uses nohup to detach the process. Returns immediately after launching.
func (c *Client) RunBackground(cmd string, logFile string) error {
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	bgCmd := fmt.Sprintf("nohup %s > %s 2>&1 &", cmd, logFile)
	c.logger.Debug().Str("cmd", bgCmd).Msg("SSH exec background")

	return session.Run(bgCmd)
}

// CheckPort checks if a UDP port is listening on the remote host.
// Returns true if the port is open, false otherwise.
func (c *Client) CheckPort(port int) (bool, error) {
	cmd := fmt.Sprintf("ss -ulnp | grep -q ':%d '", port)
	_, err := c.Run(cmd)
	if err != nil {
		return false, nil // grep didn't find match = port not open
	}
	return true, nil
}

// WaitForPort polls CheckPort until the port is listening or timeout expires.
func (c *Client) WaitForPort(port int, timeout, pollInterval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		open, _ := c.CheckPort(port)
		if open {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("timeout waiting for UDP port %d after %s", port, timeout)
}

// IsReachable tests if an SSH connection can be established.
// Does not keep the connection open.
func IsReachable(cfg ClientConfig) bool {
	if cfg.Port == 0 {
		cfg.Port = 22
	}

	// Quick TCP connectivity check first
	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return false
	}
	conn.Close()

	// Try full SSH handshake
	client, err := NewClient(cfg)
	if err != nil {
		return false
	}
	client.Close()
	return true
}
