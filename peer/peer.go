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
	Type     string `json:"type"`
	PeerName string `json:"peer_name"`
	PeerAddr string `json:"peer_addr"`
	FileName string `json:"file_name,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type Peer struct {
	Name    string
	Addr    string
	LastSeen time.Time
}

type FileTransfer struct {
	FileName string
	FileSize int64
	Progress float64
	From     string
}

type Node struct {
	Name          string
	peers         map[string]*Peer
	peersMu       sync.RWMutex
	SharedFiles   []string
	sharedMu      sync.RWMutex
	OnPeerFound   func(peer *Peer)
	OnPeerLost    func(peer *Peer)
	OnFileOffer   func(from string, fileName string, fileSize int64)
	OnTransfer    func(transfer *FileTransfer)
	OnFileReceived func(filePath string)
	running       bool
	stopChan      chan struct{}
	downloadDir   string
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

	go n.listenBroadcast()
	go n.sendBroadcast()
	go n.listenTransfer()
	go n.cleanupPeers()

	return nil
}

func (n *Node) Stop() {
	n.running = false
	close(n.stopChan)
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
	if _, err := os.Stat(filePath); err != nil {
		return err
	}

	n.sharedMu.Lock()
	n.SharedFiles = append(n.SharedFiles, filePath)
	n.sharedMu.Unlock()

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
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", BroadcastPort))
	if err != nil {
		return
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return
	}
	defer conn.Close()

	buf := make([]byte, 1024)
	for n.running {
		conn.SetReadDeadline(time.Now().Add(time.Second))
		length, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		var msg Message
		if err := json.Unmarshal(buf[:length], &msg); err != nil {
			continue
		}

		if msg.Type == "announce" && msg.PeerName != n.Name {
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
	}
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

func (n *Node) GetSharedFilesList(peerAddr string) ([]Message, error) {
	conn, err := net.DialTimeout("tcp", peerAddr, 5*time.Second)
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
	var files []Message
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
