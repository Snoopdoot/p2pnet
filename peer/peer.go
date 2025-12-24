package peer

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	BroadcastPort = 42069
	TransferPort  = 42070
	BufferSize    = 32 * 1024
)

type Message struct {
	Type       string `json:"type"`
	PeerName   string `json:"peer_name"`
	PeerAddr   string `json:"peer_addr"`
	FileName   string `json:"file_name,omitempty"`
	FileSize   int64  `json:"file_size,omitempty"`
	ReplyAddr  string `json:"reply_addr,omitempty"`
}

type SharedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type Peer struct {
	Name     string
	Addr     string
	LastSeen time.Time
}

type FileTransfer struct {
	FileName string
	FileSize int64
	Progress float64
	From     string
}

type Node struct {
	Name           string
	peers          map[string]*Peer
	peersMu        sync.RWMutex
	SharedFiles    []string
	sharedMu       sync.RWMutex
	OnPeerFound    func(peer *Peer)
	OnPeerLost     func(peer *Peer)
	OnFileOffer    func(from string, fileName string, fileSize int64)
	OnTransfer     func(transfer *FileTransfer)
	OnFileReceived func(filePath string)
	running        bool
	stopChan       chan struct{}
	downloadDir    string
	udpConn        *net.UDPConn
}

func NewNode(name string, downloadDir string) *Node {
	return &Node{
		Name:        name,
		peers:       make(map[string]*Peer),
		SharedFiles: make([]string, 0),
		stopChan:    make(chan struct{}),
		downloadDir: downloadDir,
	}
}

func (n *Node) Start() error {
	n.running = true

	// Start UDP listener for broadcasts and list requests
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", BroadcastPort))
	if err != nil {
		return err
	}
	n.udpConn, err = net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}

	go n.listenBroadcast()
	go n.sendBroadcast()
	go n.listenTransfer()
	go n.cleanupPeers()

	return nil
}

func (n *Node) Stop() {
	n.running = false
	close(n.stopChan)
	if n.udpConn != nil {
		n.udpConn.Close()
	}
}

func (n *Node) GetPeers() []*Peer {
	n.peersMu.RLock()
	defer n.peersMu.RUnlock()

	peers := make([]*Peer, 0, len(n.peers))
	for _, p := range n.peers {
		peers = append(peers, p)
	}
	return peers
}

func (n *Node) ShareFile(filePath string) error {
	stat, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	n.sharedMu.Lock()
	defer n.sharedMu.Unlock()

	if stat.IsDir() {
		return filepath.Walk(filePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() {
				n.SharedFiles = append(n.SharedFiles, path)
			}
			return nil
		})
	}

	n.SharedFiles = append(n.SharedFiles, filePath)
	return nil
}

func (n *Node) UnshareFile(filePath string) {
	n.sharedMu.Lock()
	defer n.sharedMu.Unlock()

	for i, f := range n.SharedFiles {
		if f == filePath {
			n.SharedFiles = append(n.SharedFiles[:i], n.SharedFiles[i+1:]...)
			break
		}
	}
}

func (n *Node) sendBroadcast() {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("255.255.255.255:%d", BroadcastPort))
	if err != nil {
		return
	}

	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()

	localAddr := getLocalIP()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-n.stopChan:
			return
		case <-ticker.C:
			msg := Message{
				Type:     "announce",
				PeerName: n.Name,
				PeerAddr: fmt.Sprintf("%s:%d", localAddr, TransferPort),
			}
			data, _ := json.Marshal(msg)
			conn.Write(data)
		}
	}
}

func (n *Node) listenBroadcast() {
	buf := make([]byte, 4096)
	for n.running {
		n.udpConn.SetReadDeadline(time.Now().Add(time.Second))
		length, remoteAddr, err := n.udpConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		var msg Message
		if err := json.Unmarshal(buf[:length], &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "announce":
			if msg.PeerName != n.Name {
				n.peersMu.Lock()
				isNew := n.peers[msg.PeerAddr] == nil
				n.peers[msg.PeerAddr] = &Peer{
					Name:     msg.PeerName,
					Addr:     msg.PeerAddr,
					LastSeen: time.Now(),
				}
				peer := n.peers[msg.PeerAddr]
				n.peersMu.Unlock()

				if isNew && n.OnPeerFound != nil {
					n.OnPeerFound(peer)
				}
			}

		case "list_request":
			// Someone wants our file list - connect back to them
			go n.sendFileListTo(msg.ReplyAddr, remoteAddr.IP.String())
		}
	}
}

// sendFileListTo connects to the requester and sends our file list
func (n *Node) sendFileListTo(replyAddr string, fallbackIP string) {
	// Try the provided reply address first
	conn, err := net.DialTimeout("tcp", replyAddr, 5*time.Second)
	if err != nil {
		// If reply addr fails, try using the UDP source IP with the port from replyAddr
		_, port, _ := net.SplitHostPort(replyAddr)
		altAddr := net.JoinHostPort(fallbackIP, port)
		conn, err = net.DialTimeout("tcp", altAddr, 5*time.Second)
		if err != nil {
			return
		}
	}
	defer conn.Close()

	n.sharedMu.RLock()
	files := make([]SharedFile, 0, len(n.SharedFiles))
	for _, f := range n.SharedFiles {
		stat, err := os.Stat(f)
		if err != nil {
			continue
		}
		files = append(files, SharedFile{
			Name: filepath.Base(f),
			Size: stat.Size(),
		})
	}
	n.sharedMu.RUnlock()

	encoder := json.NewEncoder(conn)
	encoder.Encode(files)
}

func (n *Node) listenTransfer() {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", TransferPort))
	if err != nil {
		return
	}
	defer listener.Close()

	for n.running {
		listener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
		conn, err := listener.Accept()
		if err != nil {
			continue
		}

		go n.handleTransfer(conn)
	}
}

func (n *Node) handleTransfer(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	var msg Message
	if err := decoder.Decode(&msg); err != nil {
		return
	}

	switch msg.Type {
	case "request":
		n.sharedMu.RLock()
		var filePath string
		for _, f := range n.SharedFiles {
			if filepath.Base(f) == msg.FileName {
				filePath = f
				break
			}
		}
		n.sharedMu.RUnlock()

		if filePath == "" {
			return
		}

		file, err := os.Open(filePath)
		if err != nil {
			return
		}
		defer file.Close()

		stat, _ := file.Stat()
		response := Message{
			Type:     "sending",
			FileName: msg.FileName,
			FileSize: stat.Size(),
		}
		encoder := json.NewEncoder(conn)
		encoder.Encode(response)

		io.Copy(conn, file)

	case "offer":
		if n.OnFileOffer != nil {
			n.OnFileOffer(msg.PeerName, msg.FileName, msg.FileSize)
		}

	case "list":
		// Keep old method as fallback
		n.sharedMu.RLock()
		files := make([]SharedFile, 0, len(n.SharedFiles))
		for _, f := range n.SharedFiles {
			stat, err := os.Stat(f)
			if err != nil {
				continue
			}
			files = append(files, SharedFile{
				Name: filepath.Base(f),
				Size: stat.Size(),
			})
		}
		n.sharedMu.RUnlock()

		encoder := json.NewEncoder(conn)
		encoder.Encode(files)
	}
}

func (n *Node) RequestFile(peerAddr string, fileName string) error {
	conn, err := net.DialTimeout("tcp", peerAddr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	msg := Message{
		Type:     "request",
		PeerName: n.Name,
		FileName: fileName,
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(msg); err != nil {
		return err
	}

	decoder := json.NewDecoder(conn)
	var response Message
	if err := decoder.Decode(&response); err != nil {
		return err
	}

	if response.Type != "sending" {
		return fmt.Errorf("unexpected response: %s", response.Type)
	}

	destPath := filepath.Join(n.downloadDir, response.FileName)
	file, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer file.Close()

	transfer := &FileTransfer{
		FileName: response.FileName,
		FileSize: response.FileSize,
		Progress: 0,
		From:     peerAddr,
	}

	buf := make([]byte, BufferSize)
	var received int64 = 0

	for received < response.FileSize {
		nr, err := conn.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		nw, err := file.Write(buf[:nr])
		if err != nil {
			return err
		}
		if nw != nr {
			return fmt.Errorf("short write")
		}
		received += int64(nr)
		transfer.Progress = float64(received) / float64(response.FileSize)
		if n.OnTransfer != nil {
			n.OnTransfer(transfer)
		}
	}

	if n.OnFileReceived != nil {
		n.OnFileReceived(destPath)
	}

	return nil
}

// GetSharedFilesList uses reverse connection - we open a listener and ask the peer to connect to us
func (n *Node) GetSharedFilesList(peerAddr string) ([]SharedFile, error) {
	// First try direct connection (works if their firewall allows it)
	files, err := n.getFilesListDirect(peerAddr)
	if err == nil {
		return files, nil
	}

	// Direct failed, use reverse connection
	return n.getFilesListReverse(peerAddr)
}

func (n *Node) getFilesListDirect(peerAddr string) ([]SharedFile, error) {
	conn, err := net.DialTimeout("tcp", peerAddr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	msg := Message{
		Type:     "list",
		PeerName: n.Name,
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(msg); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(conn)
	var files []SharedFile
	if err := decoder.Decode(&files); err != nil {
		return nil, err
	}

	return files, nil
}

func (n *Node) getFilesListReverse(peerAddr string) ([]SharedFile, error) {
	// Open a temporary listener on random port
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	// Get the port we're listening on
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	localIP := getLocalIP()
	replyAddr := net.JoinHostPort(localIP, port)

	// Send UDP request to peer asking them to connect back
	peerHost, _, _ := net.SplitHostPort(peerAddr)
	udpAddr, err := net.ResolveUDPAddr("udp4", net.JoinHostPort(peerHost, fmt.Sprintf("%d", BroadcastPort)))
	if err != nil {
		return nil, err
	}

	msg := Message{
		Type:      "list_request",
		PeerName:  n.Name,
		ReplyAddr: replyAddr,
	}
	data, _ := json.Marshal(msg)

	sendConn, err := net.DialUDP("udp4", nil, udpAddr)
	if err != nil {
		return nil, err
	}
	sendConn.Write(data)
	sendConn.Close()

	// Wait for incoming connection with timeout
	listener.(*net.TCPListener).SetDeadline(time.Now().Add(5 * time.Second))
	conn, err := listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("peer did not connect back: %v", err)
	}
	defer conn.Close()

	// Read the file list
	decoder := json.NewDecoder(conn)
	var files []SharedFile
	if err := decoder.Decode(&files); err != nil {
		return nil, err
	}

	return files, nil
}

func (n *Node) cleanupPeers() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-n.stopChan:
			return
		case <-ticker.C:
			n.peersMu.Lock()
			for addr, peer := range n.peers {
				if time.Since(peer.LastSeen) > 10*time.Second {
					delete(n.peers, addr)
					if n.OnPeerLost != nil {
						n.OnPeerLost(peer)
					}
				}
			}
			n.peersMu.Unlock()
		}
	}
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}
