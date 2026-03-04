package validator

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/rs/zerolog/log"
	internalssh "github.com/sol-strategies/solana-validator-failover/internal/ssh"
	"github.com/sol-strategies/solana-validator-failover/internal/style"
	"github.com/sol-strategies/solana-validator-failover/internal/utils"
)

// SwitchoverParams holds the parameters for the switchover command
type SwitchoverParams struct {
	AutoConfirm bool   // --yes: skip interactive menu
	DryRun      bool   // --dry-run: simulate without switching
	ToPeer      string // --to-peer: specify target peer
}

// NodeState holds the discovered state of a single node in the cluster
type NodeState struct {
	Name     string
	IP       string
	Identity string // gossip pubkey from getIdentity RPC
	Role     string // "ACTIVE", "PASSIVE", "DOWN", "UNKNOWN"
	Health   string // "ok", "behind", "down"
	Slot     uint64
	SSHOk    bool // whether SSH is reachable (always true for local)
	IsLocal  bool // whether this is the node we're running on
}

// ClusterState holds the discovered state of the entire cluster
type ClusterState struct {
	Nodes       map[string]*NodeState
	ActiveNode  string // name of the active node (empty if none found)
	PassiveNode string // name of the passive node (empty if none found)
	SlotLag     uint64
}

// SwitchoverOrchestrator manages the switchover process
type SwitchoverOrchestrator struct {
	peers           Peers
	switchoverCfg   SwitchoverConfig
	localName       string // this node's name (from config hostname)
	localIP         string // this node's public IP
	localRPCAddress string
	activePubkey    string
	passivePubkey   string
	configPath      string // config file path (for building remote commands)
	quicPort        int
}

// NewSwitchoverOrchestrator creates a SwitchoverOrchestrator from a loaded config.
// It performs minimal validation compared to NewFromConfig — it does NOT require
// ledger dir, tower file, or identity keypair files to exist on this machine.
func NewSwitchoverOrchestrator(cfg *Config, configPath string) (*SwitchoverOrchestrator, error) {
	o := &SwitchoverOrchestrator{
		configPath: configPath,
	}

	// Parse peers
	if len(cfg.Failover.Peers) == 0 {
		return nil, fmt.Errorf("must have at least one peer configured")
	}

	o.peers = make(Peers)
	for name, peer := range cfg.Failover.Peers {
		if !utils.IsValidURLWithPort(peer.Address) {
			return nil, fmt.Errorf("invalid peer address %s for peer %s", peer.Address, name)
		}
		sshUser := peer.SSHUser
		if sshUser == "" {
			sshUser = "solana"
		}
		sshPort := peer.SSHPort
		if sshPort == 0 {
			sshPort = 22
		}
		rpcURL := peer.RPCURL
		if rpcURL == "" {
			rpcURL = "http://127.0.0.1:8899"
		}
		o.peers[name] = Peer{
			Name:    name,
			Address: peer.Address,
			SSHUser: sshUser,
			SSHKey:  peer.SSHKey,
			SSHPort: sshPort,
			RPCURL:  rpcURL,
		}
	}

	// Load identity pubkeys (supports pubkey-only mode)
	if cfg.Identities.Active != "" {
		// Try loading from file to get pubkey
		ident, err := loadPubkeyFromFile(cfg.Identities.Active)
		if err != nil {
			return nil, fmt.Errorf("failed to load active identity pubkey from file: %w", err)
		}
		o.activePubkey = ident
	} else if cfg.Identities.ActivePubkey != "" {
		o.activePubkey = cfg.Identities.ActivePubkey
	} else {
		return nil, fmt.Errorf("either identities.active or identities.active_pubkey must be set")
	}

	if cfg.Identities.Passive != "" {
		ident, err := loadPubkeyFromFile(cfg.Identities.Passive)
		if err != nil {
			return nil, fmt.Errorf("failed to load passive identity pubkey from file: %w", err)
		}
		o.passivePubkey = ident
	} else if cfg.Identities.PassivePubkey != "" {
		o.passivePubkey = cfg.Identities.PassivePubkey
	} else {
		return nil, fmt.Errorf("either identities.passive or identities.passive_pubkey must be set")
	}

	// Local RPC address
	o.localRPCAddress = cfg.RPCAddress
	if o.localRPCAddress == "" {
		o.localRPCAddress = "http://127.0.0.1:8899"
	}

	// QUIC port
	o.quicPort = cfg.Failover.Server.Port
	if o.quicPort == 0 {
		o.quicPort = 9898
	}

	// Switchover config
	o.switchoverCfg = cfg.Failover.Switchover
	if o.switchoverCfg.MaxSlotLag == 0 {
		o.switchoverCfg.MaxSlotLag = 100
	}
	if o.switchoverCfg.FailoverBinary == "" {
		o.switchoverCfg.FailoverBinary = "solana-validator-failover"
	}

	// Detect local node identity — use config hostname (not peer matching)
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	if cfg.Hostname != "" {
		hostname = cfg.Hostname
	}
	o.localName = hostname
	o.localIP = cfg.PublicIP

	log.Debug().
		Str("local_name", o.localName).
		Str("active_pubkey", o.activePubkey).
		Str("passive_pubkey", o.passivePubkey).
		Int("quic_port", o.quicPort).
		Msg("switchover orchestrator initialized")

	return o, nil
}

// loadPubkeyFromFile reads a Solana keypair JSON file and derives the public key.
// The file contains a JSON array of 64 bytes (32-byte private key + 32-byte public key).
func loadPubkeyFromFile(path string) (string, error) {
	resolved, err := utils.ResolvePath(path)
	if err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}

	var keyBytes []byte
	if err := json.Unmarshal(data, &keyBytes); err != nil {
		return "", fmt.Errorf("invalid keypair format: %w", err)
	}

	if len(keyBytes) != 64 {
		return "", fmt.Errorf("keypair must be 64 bytes, got %d", len(keyBytes))
	}

	// Public key is the last 32 bytes — encode as base58
	pubKeyBytes := keyBytes[32:]
	return base58Encode(pubKeyBytes), nil
}

// base58Encode encodes bytes to base58 (Bitcoin alphabet)
func base58Encode(input []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

	// Count leading zeros
	var leadingZeros int
	for _, b := range input {
		if b != 0 {
			break
		}
		leadingZeros++
	}

	// Convert to big integer and encode
	size := len(input)*138/100 + 1
	buf := make([]byte, size)
	var length int

	for _, b := range input {
		carry := int(b)
		for j := size - 1; j >= 0; j-- {
			carry += 256 * int(buf[j])
			buf[j] = byte(carry % 58)
			carry /= 58
			if carry == 0 && j <= size-1-length {
				break
			}
		}
		length = size
		for length > 0 && buf[size-length] == 0 {
			length--
		}
	}

	var result strings.Builder
	for i := 0; i < leadingZeros; i++ {
		result.WriteByte('1')
	}
	for i := size - length; i < size; i++ {
		result.WriteByte(alphabet[buf[i]])
	}
	return result.String()
}

// Switchover is the main entry point. Orchestrates the entire flow.
func (o *SwitchoverOrchestrator) Switchover(params SwitchoverParams) error {
	fmt.Println()
	title := lipgloss.NewStyle().Bold(true).Foreground(style.ColorPurple).
		Render("  Solana Validator Planned Switchover")
	fmt.Println(title)
	fmt.Println()

	// 1. Discover cluster state
	state, err := o.DiscoverClusterState()
	if err != nil {
		return fmt.Errorf("discovery failed: %w", err)
	}

	// 2. Show dashboard
	fmt.Println(o.RenderDashboard(state))

	// 3. Edge cases
	activeCount := 0
	for _, node := range state.Nodes {
		if node.Role == "ACTIVE" {
			activeCount++
		}
	}

	if activeCount >= 2 {
		return fmt.Errorf("SPLIT-BRAIN: multiple nodes report ACTIVE identity! Manual intervention required")
	}
	if activeCount == 0 {
		fmt.Println(style.RenderWarningString("  No ACTIVE node found. All nodes are passive."))
		fmt.Println(style.RenderGreyString("  HA daemon will auto-promote when a node is healthy.", false))
		return nil
	}
	if state.ActiveNode == "" || state.PassiveNode == "" {
		return fmt.Errorf("could not identify both ACTIVE and PASSIVE nodes")
	}

	// 4. Determine action
	choice := 0 // 1=live, 2=dry-run
	if params.DryRun {
		choice = 2
	} else if params.AutoConfirm {
		choice = 1
	} else {
		choice, err = o.ShowMenu(state)
		if err != nil {
			return err
		}
	}

	switch choice {
	case 0:
		fmt.Println(style.RenderGreyString("  Bye", false))
		return nil
	case 3:
		o.ShowManualCommands(state)
		return nil
	case 1, 2:
		isDryRun := choice == 2
		switchoverParams := SwitchoverParams{
			AutoConfirm: true,
			DryRun:      isDryRun,
			ToPeer:      params.ToPeer,
		}

		// 5. Pre-flight checks
		if err := o.RunPreflightChecks(state, isDryRun); err != nil {
			if !isDryRun {
				return fmt.Errorf("pre-flight failed: %w", err)
			}
			fmt.Println(style.RenderWarningString("  Pre-flight has warnings, proceeding with dry-run..."))
		}

		// 6. Confirm
		if !params.AutoConfirm && !params.DryRun {
			modeLabel := "LIVE"
			if isDryRun {
				modeLabel = "DRY-RUN"
			}
			fmt.Println()
			fmt.Printf("  Planned switchover: %s → %s  [%s]\n",
				style.RenderActiveString(state.ActiveNode, true),
				style.RenderPassiveString(state.PassiveNode, true),
				modeLabel,
			)
			fmt.Println()

			var confirm bool
			err = huh.NewConfirm().
				Title("Proceed with switchover?").
				Value(&confirm).
				Run()
			if err != nil || !confirm {
				fmt.Println(style.RenderGreyString("  Cancelled", false))
				return nil
			}
		}

		// 7. Execute
		if err := o.ExecuteSwitchover(state, switchoverParams); err != nil {
			return err
		}

		// 8. Verify
		return o.VerifyPostSwitchover()
	}

	return nil
}

// DiscoverClusterState queries all nodes to build a complete cluster picture.
func (o *SwitchoverOrchestrator) DiscoverClusterState() (*ClusterState, error) {
	fmt.Println(style.RenderGreyString("  Discovering cluster state...", false))
	fmt.Println()

	state := &ClusterState{
		Nodes: make(map[string]*NodeState),
	}

	// Local node
	if o.localName != "" {
		localNode, err := o.discoverLocalNode()
		if err != nil {
			log.Warn().Err(err).Msg("local RPC not responding")
			localNode = &NodeState{
				Name:    o.localName,
				Role:    "DOWN",
				Health:  "down",
				IsLocal: true,
				SSHOk:   true,
			}
		}
		state.Nodes[o.localName] = localNode
		o.printNodeStatus(localNode, "(local)")
	}

	// Remote peers
	for name, peer := range o.peers {
		if name == o.localName {
			continue
		}

		peerHost, _, _ := net.SplitHostPort(peer.Address)
		node := &NodeState{
			Name:  name,
			IP:    peerHost,
			SSHOk: false,
		}

		// Resolve SSH key path
		sshKeyPath := peer.SSHKey
		if sshKeyPath != "" {
			resolved, err := utils.ResolvePath(sshKeyPath)
			if err == nil {
				sshKeyPath = resolved
			}
		}

		if sshKeyPath == "" {
			fmt.Printf("  %s %s (%s): SSH key not configured\n",
				style.RenderErrorString("✗"), name, peerHost)
			node.Role = "DOWN"
			node.Health = "down"
			state.Nodes[name] = node
			continue
		}

		// SSH to peer
		sshClient, err := internalssh.NewClient(internalssh.ClientConfig{
			Host:    peerHost,
			Port:    peer.SSHPort,
			User:    peer.SSHUser,
			KeyFile: sshKeyPath,
		})
		if err != nil {
			fmt.Printf("  %s %s (%s): SSH unreachable — %s\n",
				style.RenderErrorString("✗"), name, peerHost, err)
			node.Role = "DOWN"
			node.Health = "down"
			state.Nodes[name] = node
			continue
		}
		defer sshClient.Close()
		node.SSHOk = true

		// RPC via SSH
		node.Identity, _ = o.getIdentityViaSSH(sshClient, peer.RPCURL)
		node.Health, _ = o.getHealthViaSSH(sshClient, peer.RPCURL)
		node.Slot, _ = o.getSlotViaSSH(sshClient, peer.RPCURL)
		node.Role = o.determineRole(node.Identity)

		state.Nodes[name] = node
		o.printNodeStatus(node, fmt.Sprintf("(%s)", peerHost))
	}

	// Determine active and passive
	for name, node := range state.Nodes {
		switch node.Role {
		case "ACTIVE":
			state.ActiveNode = name
		case "PASSIVE":
			state.PassiveNode = name
		}
	}

	// Calculate slot lag
	var maxSlot, minSlot uint64
	first := true
	for _, node := range state.Nodes {
		if node.Slot == 0 {
			continue
		}
		if first {
			maxSlot = node.Slot
			minSlot = node.Slot
			first = false
		} else {
			if node.Slot > maxSlot {
				maxSlot = node.Slot
			}
			if node.Slot < minSlot {
				minSlot = node.Slot
			}
		}
	}
	state.SlotLag = maxSlot - minSlot

	return state, nil
}

func (o *SwitchoverOrchestrator) printNodeStatus(node *NodeState, suffix string) {
	if node.Role == "DOWN" {
		fmt.Printf("  %s %s %s: RPC not responding\n",
			style.RenderWarningString("⚠"), node.Name, suffix)
	} else {
		roleStr := style.RenderActiveString(node.Role, true)
		if node.Role == "PASSIVE" {
			roleStr = style.RenderPassiveString(node.Role, false)
		}
		healthStr := style.RenderActiveString(node.Health, false)
		if node.Health != "ok" {
			healthStr = style.RenderWarningString(node.Health)
		}
		fmt.Printf("  %s %s %s: %s — health: %s\n",
			style.RenderActiveString("✓", false), node.Name, suffix, roleStr, healthStr)
	}
}

func (o *SwitchoverOrchestrator) discoverLocalNode() (*NodeState, error) {
	node := &NodeState{
		Name:    o.localName,
		IP:      o.localIP,
		IsLocal: true,
		SSHOk:   true,
	}

	identity, err := o.getIdentityLocal()
	if err != nil {
		return nil, err
	}
	node.Identity = identity
	node.Role = o.determineRole(identity)

	health, err := o.getHealthLocal()
	if err != nil {
		node.Health = "down"
	} else {
		node.Health = health
	}

	slot, err := o.getSlotLocal()
	if err != nil {
		node.Slot = 0
	} else {
		node.Slot = slot
	}

	return node, nil
}

func (o *SwitchoverOrchestrator) determineRole(identityPubkey string) string {
	switch identityPubkey {
	case o.activePubkey:
		return "ACTIVE"
	case "":
		return "DOWN"
	default:
		if identityPubkey == o.passivePubkey {
			return "PASSIVE"
		}
		return "UNKNOWN"
	}
}

// RPC queries — local (direct HTTP)

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
}

type identityResult struct {
	Identity string `json:"identity"`
}

func (o *SwitchoverOrchestrator) getIdentityLocal() (string, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"getIdentity"}`
	resp, err := http.Post(o.localRPCAddress, "application/json", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var rpc rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return "", err
	}

	var result identityResult
	if err := json.Unmarshal(rpc.Result, &result); err != nil {
		return "", err
	}
	return result.Identity, nil
}

func (o *SwitchoverOrchestrator) getHealthLocal() (string, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(o.localRPCAddress + "/health")
	if err != nil {
		return "down", err
	}
	defer resp.Body.Close()

	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	result := strings.TrimSpace(string(buf[:n]))
	if result == "ok" {
		return "ok", nil
	}
	if result != "" {
		return "behind", nil
	}
	return "down", nil
}

func (o *SwitchoverOrchestrator) getSlotLocal() (uint64, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}`
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(o.localRPCAddress, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var rpc rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		return 0, err
	}

	var slot uint64
	if err := json.Unmarshal(rpc.Result, &slot); err != nil {
		return 0, err
	}
	return slot, nil
}

// RPC queries — remote (via SSH + curl)

func (o *SwitchoverOrchestrator) getIdentityViaSSH(client *internalssh.Client, rpcURL string) (string, error) {
	cmd := fmt.Sprintf(`curl -s --max-time 5 -X POST %s -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"getIdentity"}' | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['identity'])"`, rpcURL)
	output, err := client.Run(cmd)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

func (o *SwitchoverOrchestrator) getHealthViaSSH(client *internalssh.Client, rpcURL string) (string, error) {
	cmd := fmt.Sprintf("curl -s --max-time 5 %s/health", rpcURL)
	output, err := client.Run(cmd)
	if err != nil {
		return "down", nil
	}
	result := strings.TrimSpace(output)
	if result == "ok" {
		return "ok", nil
	}
	if result != "" {
		return "behind", nil
	}
	return "down", nil
}

func (o *SwitchoverOrchestrator) getSlotViaSSH(client *internalssh.Client, rpcURL string) (uint64, error) {
	cmd := fmt.Sprintf(`curl -s --max-time 5 -X POST %s -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}' | python3 -c "import sys,json; print(json.load(sys.stdin)['result'])"`, rpcURL)
	output, err := client.Run(cmd)
	if err != nil {
		return 0, err
	}
	slot, err := strconv.ParseUint(strings.TrimSpace(output), 10, 64)
	if err != nil {
		return 0, err
	}
	return slot, nil
}

// RenderDashboard renders a lipgloss-styled table showing the cluster state.
func (o *SwitchoverOrchestrator) RenderDashboard(state *ClusterState) string {
	headers := []string{"Node", "IP", "Role", "Health", "Slot"}

	// Sort node names for consistent display
	names := make([]string, 0, len(state.Nodes))
	for name := range state.Nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	rows := make([][]string, 0, len(names))
	for _, name := range names {
		node := state.Nodes[name]
		ip := node.IP
		if node.IsLocal {
			ip += " (local)"
		}
		rows = append(rows, []string{
			name,
			ip,
			node.Role,
			node.Health,
			fmt.Sprintf("%d", node.Slot),
		})
	}

	// Custom style function for role coloring
	styleFunc := func(row, col int) lipgloss.Style {
		if row == 0 { // header
			return style.TableHeaderStyle
		}
		baseStyle := style.TableCellStyle

		// Color the Role column
		if col == 2 && row > 0 && row-1 < len(rows) {
			role := rows[row-1][2]
			switch role {
			case "ACTIVE":
				return baseStyle.Foreground(style.ColorActive).Bold(true)
			case "PASSIVE":
				return baseStyle.Foreground(style.ColorPassive)
			case "DOWN":
				return baseStyle.Foreground(style.ColorErrorValue)
			}
		}
		// Color the Health column
		if col == 3 && row > 0 && row-1 < len(rows) {
			health := rows[row-1][3]
			switch health {
			case "ok":
				return baseStyle.Foreground(style.ColorActive)
			case "behind":
				return baseStyle.Foreground(style.ColorWarning)
			case "down":
				return baseStyle.Foreground(style.ColorErrorValue)
			}
		}
		return baseStyle
	}

	result := "\n" + style.RenderTable(headers, rows, styleFunc)

	if state.SlotLag > uint64(o.switchoverCfg.MaxSlotLag) {
		result += fmt.Sprintf("\n  Slot lag: %s (threshold: %d)",
			style.RenderErrorString(fmt.Sprintf("%d", state.SlotLag)),
			o.switchoverCfg.MaxSlotLag)
	} else if state.SlotLag > 0 {
		result += fmt.Sprintf("\n  Slot lag: %s",
			style.RenderActiveString(fmt.Sprintf("%d", state.SlotLag), false))
	}

	return result + "\n"
}

// RunPreflightChecks validates the cluster state is suitable for switchover.
func (o *SwitchoverOrchestrator) RunPreflightChecks(state *ClusterState, dryRun bool) error {
	fmt.Println()
	fmt.Println(style.RenderGreyString("  Running pre-flight checks...", false))
	fmt.Println()

	errors := 0

	// Active node found
	if state.ActiveNode == "" {
		fmt.Printf("  %s No ACTIVE node found\n", style.RenderErrorString("✗"))
		errors++
	} else {
		fmt.Printf("  %s Active node: %s\n", style.RenderActiveString("✓", false), state.ActiveNode)
	}

	// Passive node found
	if state.PassiveNode == "" {
		fmt.Printf("  %s No PASSIVE node found\n", style.RenderErrorString("✗"))
		errors++
	} else {
		fmt.Printf("  %s Passive node: %s\n", style.RenderActiveString("✓", false), state.PassiveNode)
	}

	// Health checks
	if state.ActiveNode != "" {
		activeNode := state.Nodes[state.ActiveNode]
		if activeNode.Health != "ok" {
			fmt.Printf("  %s Active node health: %s\n", style.RenderWarningString("⚠"), activeNode.Health)
			if !dryRun {
				errors++
			}
		} else {
			fmt.Printf("  %s Active node healthy\n", style.RenderActiveString("✓", false))
		}
	}

	if state.PassiveNode != "" {
		passiveNode := state.Nodes[state.PassiveNode]
		if passiveNode.Health != "ok" {
			fmt.Printf("  %s Passive node health: %s\n", style.RenderWarningString("⚠"), passiveNode.Health)
			if !dryRun {
				errors++
			}
		} else {
			fmt.Printf("  %s Passive node healthy\n", style.RenderActiveString("✓", false))
		}
	}

	// Slot lag
	if state.SlotLag > uint64(o.switchoverCfg.MaxSlotLag) {
		fmt.Printf("  %s Slot lag too high: %d (max: %d)\n",
			style.RenderErrorString("✗"), state.SlotLag, o.switchoverCfg.MaxSlotLag)
		if !dryRun {
			errors++
		}
	} else {
		fmt.Printf("  %s Slot lag: %d\n", style.RenderActiveString("✓", false), state.SlotLag)
	}

	// SSH availability for remote nodes
	if state.ActiveNode != "" && state.ActiveNode != o.localName {
		activeNode := state.Nodes[state.ActiveNode]
		if !activeNode.SSHOk {
			fmt.Printf("  %s SSH to active node %s unreachable\n", style.RenderErrorString("✗"), state.ActiveNode)
			errors++
		}
	}
	if state.PassiveNode != "" && state.PassiveNode != o.localName {
		passiveNode := state.Nodes[state.PassiveNode]
		if !passiveNode.SSHOk {
			fmt.Printf("  %s SSH to passive node %s unreachable\n", style.RenderErrorString("✗"), state.PassiveNode)
			errors++
		}
	}

	fmt.Println()
	if errors > 0 {
		fmt.Printf("  %s Pre-flight failed with %d error(s)\n", style.RenderErrorString("✗"), errors)
		return fmt.Errorf("%d pre-flight check(s) failed", errors)
	}
	fmt.Printf("  %s All pre-flight checks passed\n", style.RenderActiveString("✓", false))
	return nil
}

// ShowMenu displays the interactive switchover menu.
func (o *SwitchoverOrchestrator) ShowMenu(state *ClusterState) (int, error) {
	fmt.Println()

	sshOk := true
	if state.ActiveNode != "" && state.ActiveNode != o.localName {
		if node, ok := state.Nodes[state.ActiveNode]; ok && !node.SSHOk {
			sshOk = false
		}
	}
	if state.PassiveNode != "" && state.PassiveNode != o.localName {
		if node, ok := state.Nodes[state.PassiveNode]; ok && !node.SSHOk {
			sshOk = false
		}
	}

	options := make([]huh.Option[int], 0)
	if state.ActiveNode != "" && state.PassiveNode != "" && sshOk {
		options = append(options,
			huh.NewOption(
				fmt.Sprintf("Planned switchover: %s → %s",
					style.RenderActiveString(state.ActiveNode, true),
					style.RenderPassiveString(state.PassiveNode, true)),
				1,
			),
			huh.NewOption("Dry-run (simulate without switching)", 2),
		)
	}
	options = append(options,
		huh.NewOption("Show manual commands (copy-paste)", 3),
		huh.NewOption("Exit", 0),
	)

	var choice int
	err := huh.NewSelect[int]().
		Title("Choose an action:").
		Options(options...).
		Value(&choice).
		Run()
	if err != nil {
		return 0, err
	}

	return choice, nil
}

// ShowManualCommands prints copy-pasteable manual switchover commands.
func (o *SwitchoverOrchestrator) ShowManualCommands(state *ClusterState) {
	fmt.Println()
	fmt.Println(style.RenderPurpleString("  Manual switchover commands:"))
	fmt.Println()

	names := make([]string, 0, len(o.peers))
	for name := range o.peers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, from := range names {
		for _, to := range names {
			if from == to {
				continue
			}
			fromPeer := o.peers[from]
			toPeer := o.peers[to]
			fromHost, _, _ := net.SplitHostPort(fromPeer.Address)
			toHost, _, _ := net.SplitHostPort(toPeer.Address)

			fmt.Printf("  %s → %s:\n", style.RenderActiveString(from, true), style.RenderPassiveString(to, true))
			fmt.Printf("    Step 1 — On %s (%s), start QUIC server:\n", to, toHost)
			fmt.Printf("      %s run -c <config> --yes --not-a-drill\n", o.switchoverCfg.FailoverBinary)
			fmt.Printf("    Step 2 — On %s (%s), start QUIC client:\n", from, fromHost)
			fmt.Printf("      %s run -c <config> --yes --to-peer %s --not-a-drill\n",
				o.switchoverCfg.FailoverBinary, to)
			fmt.Println()
		}
	}
}

// ExecuteSwitchover performs the actual switchover orchestration.
func (o *SwitchoverOrchestrator) ExecuteSwitchover(state *ClusterState, params SwitchoverParams) error {
	modeLabel := "LIVE"
	if params.DryRun {
		modeLabel = "DRY-RUN"
	}

	fmt.Println()
	fmt.Printf("  Planned switchover: %s → %s  [%s]\n",
		style.RenderActiveString(state.ActiveNode, true),
		style.RenderPassiveString(state.PassiveNode, true),
		modeLabel,
	)
	fmt.Println()

	// Detect which case we're in
	if o.localName == state.PassiveNode {
		return o.executeFromPassive(state, params)
	} else if o.localName == state.ActiveNode {
		return o.executeFromActive(state, params)
	} else {
		return o.executeFromExternal(state, params)
	}
}

// buildServerCommand constructs the run command for the passive (server) side
func (o *SwitchoverOrchestrator) buildServerCommand(dryRun bool) string {
	cmd := fmt.Sprintf("%s run -c %s --yes", o.switchoverCfg.FailoverBinary, o.configPath)
	if !dryRun {
		cmd += " --not-a-drill"
	}
	return cmd
}

// buildClientCommand constructs the run command for the active (client) side
func (o *SwitchoverOrchestrator) buildClientCommand(passivePeer string, dryRun bool) string {
	cmd := fmt.Sprintf("%s run -c %s --yes --to-peer %s", o.switchoverCfg.FailoverBinary, o.configPath, passivePeer)
	if !dryRun {
		cmd += " --not-a-drill"
	}
	return cmd
}

// newSSHClient creates an SSH client to a peer
func (o *SwitchoverOrchestrator) newSSHClient(peer Peer) (*internalssh.Client, error) {
	sshKeyPath := peer.SSHKey
	if sshKeyPath != "" {
		resolved, err := utils.ResolvePath(sshKeyPath)
		if err == nil {
			sshKeyPath = resolved
		}
	}
	host, _, _ := net.SplitHostPort(peer.Address)
	return internalssh.NewClient(internalssh.ClientConfig{
		Host:    host,
		Port:    peer.SSHPort,
		User:    peer.SSHUser,
		KeyFile: sshKeyPath,
	})
}

// Case 1: Running from PASSIVE node
func (o *SwitchoverOrchestrator) executeFromPassive(state *ClusterState, params SwitchoverParams) error {
	fmt.Println(style.RenderGreyString("  Running from PASSIVE node — server locally, client via SSH", false))
	fmt.Println()

	activePeer := o.peers[state.ActiveNode]
	activeHost, _, _ := net.SplitHostPort(activePeer.Address)

	// Start client on active peer via SSH (background)
	fmt.Printf("  Starting QUIC client on %s (%s) via SSH...\n",
		style.RenderActiveString(state.ActiveNode, true), activeHost)

	clientCmd := o.buildClientCommand(state.PassiveNode, params.DryRun)
	sshClient, err := o.newSSHClient(activePeer)
	if err != nil {
		return fmt.Errorf("SSH to active node %s failed: %w", state.ActiveNode, err)
	}
	// Run client in background (it will connect when server is ready)
	if err := sshClient.RunBackground(clientCmd, "/tmp/switchover-client.log"); err != nil {
		sshClient.Close()
		return fmt.Errorf("failed to start client on %s: %w", state.ActiveNode, err)
	}
	sshClient.Close()

	time.Sleep(2 * time.Second)

	// Run server locally
	fmt.Printf("  Starting QUIC server locally...\n")
	serverCmd := o.buildServerCommand(params.DryRun)
	if err := o.runLocalCommand(serverCmd); err != nil {
		return fmt.Errorf("local server failed: %w", err)
	}

	return nil
}

// Case 2: Running from ACTIVE node
func (o *SwitchoverOrchestrator) executeFromActive(state *ClusterState, params SwitchoverParams) error {
	fmt.Println(style.RenderGreyString("  Running from ACTIVE node — server on peer, client locally", false))
	fmt.Println()

	passivePeer := o.peers[state.PassiveNode]
	passiveHost, _, _ := net.SplitHostPort(passivePeer.Address)

	// Start server on passive peer via SSH (background)
	fmt.Printf("  Starting QUIC server on %s (%s) via SSH...\n",
		style.RenderPassiveString(state.PassiveNode, true), passiveHost)

	serverCmd := o.buildServerCommand(params.DryRun)
	sshClient, err := o.newSSHClient(passivePeer)
	if err != nil {
		return fmt.Errorf("SSH to passive node %s failed: %w", state.PassiveNode, err)
	}

	if err := sshClient.RunBackground(serverCmd, "/tmp/switchover-server.log"); err != nil {
		sshClient.Close()
		return fmt.Errorf("failed to start server on %s: %w", state.PassiveNode, err)
	}

	// Wait for QUIC port
	fmt.Printf("  Waiting for QUIC server on %s:%d...\n", passiveHost, o.quicPort)
	err = sshClient.WaitForPort(o.quicPort, 30*time.Second, 1*time.Second)
	sshClient.Close()
	if err != nil {
		return fmt.Errorf("timeout: QUIC server didn't start on %s — check logs: ssh %s@%s cat /tmp/switchover-server.log",
			state.PassiveNode, passivePeer.SSHUser, passiveHost)
	}
	fmt.Printf("  %s QUIC server is listening\n", style.RenderActiveString("✓", false))

	// Run client locally
	fmt.Printf("  Starting QUIC client locally...\n")
	clientCmd := o.buildClientCommand(state.PassiveNode, params.DryRun)
	if err := o.runLocalCommand(clientCmd); err != nil {
		return fmt.Errorf("local client failed: %w — server logs: ssh %s@%s cat /tmp/switchover-server.log",
			err, passivePeer.SSHUser, passiveHost)
	}

	return nil
}

// Case 3: Running from external node
func (o *SwitchoverOrchestrator) executeFromExternal(state *ClusterState, params SwitchoverParams) error {
	fmt.Println(style.RenderGreyString("  Running from external node — both sides via SSH", false))
	fmt.Println()

	passivePeer := o.peers[state.PassiveNode]
	activePeer := o.peers[state.ActiveNode]
	passiveHost, _, _ := net.SplitHostPort(passivePeer.Address)
	activeHost, _, _ := net.SplitHostPort(activePeer.Address)

	// Start server on passive peer (background)
	fmt.Printf("  Starting QUIC server on %s (%s) via SSH...\n",
		style.RenderPassiveString(state.PassiveNode, true), passiveHost)

	serverSSH, err := o.newSSHClient(passivePeer)
	if err != nil {
		return fmt.Errorf("SSH to passive node %s failed: %w", state.PassiveNode, err)
	}

	serverCmd := o.buildServerCommand(params.DryRun)
	if err := serverSSH.RunBackground(serverCmd, "/tmp/switchover-server.log"); err != nil {
		serverSSH.Close()
		return fmt.Errorf("failed to start server on %s: %w", state.PassiveNode, err)
	}

	// Wait for QUIC port
	fmt.Printf("  Waiting for QUIC server on %s:%d...\n", passiveHost, o.quicPort)
	err = serverSSH.WaitForPort(o.quicPort, 30*time.Second, 1*time.Second)
	serverSSH.Close()
	if err != nil {
		return fmt.Errorf("timeout: QUIC server didn't start on %s", state.PassiveNode)
	}
	fmt.Printf("  %s QUIC server is listening\n", style.RenderActiveString("✓", false))

	// Start client on active peer (foreground via SSH — streams output)
	fmt.Printf("  Starting QUIC client on %s (%s) via SSH...\n",
		style.RenderActiveString(state.ActiveNode, true), activeHost)

	clientSSH, err := o.newSSHClient(activePeer)
	if err != nil {
		return fmt.Errorf("SSH to active node %s failed: %w", state.ActiveNode, err)
	}
	defer clientSSH.Close()

	clientCmd := o.buildClientCommand(state.PassiveNode, params.DryRun)
	if err := clientSSH.RunStreaming(clientCmd, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("client on %s failed: %w", state.ActiveNode, err)
	}

	return nil
}

// runLocalCommand executes the failover binary locally as a subprocess
func (o *SwitchoverOrchestrator) runLocalCommand(cmdStr string) error {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	log.Debug().Str("cmd", cmdStr).Msg("executing local command")
	return cmd.Run()
}

// VerifyPostSwitchover re-discovers cluster state after switchover.
func (o *SwitchoverOrchestrator) VerifyPostSwitchover() error {
	fmt.Println()
	fmt.Println(style.RenderGreyString("  Verifying switchover result...", false))
	time.Sleep(5 * time.Second)

	state, err := o.DiscoverClusterState()
	if err != nil {
		log.Warn().Err(err).Msg("post-switchover verification failed")
		return nil // don't fail — switchover may have partially succeeded
	}

	fmt.Println()
	fmt.Println(style.RenderPurpleString("  Post-switchover status:"))
	for name, node := range state.Nodes {
		roleStr := style.RenderActiveString(node.Role, true)
		if node.Role == "PASSIVE" {
			roleStr = style.RenderPassiveString(node.Role, false)
		} else if node.Role == "DOWN" {
			roleStr = style.RenderErrorString(node.Role)
		}
		suffix := ""
		if node.IsLocal {
			suffix = " (local)"
		}
		fmt.Printf("  %s%s: %s\n", name, suffix, roleStr)
	}
	fmt.Println()

	return nil
}
