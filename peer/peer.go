package peer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
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

type SharedFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
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
	tlsConfig      *tls.Config
}

func generateTLSConfig() (*tls.Config, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"P2PNet"},
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certDER},
			PrivateKey:  privateKey,
			Leaf:        leaf,
		}},
		MinVersion: tls.VersionTLS12,
	}, nil
}

func NewNode(name string, downloadDir string) *Node {
	tlsConfig, err := generateTLSConfig()
	if err != nil {
		panic("failed to generate TLS config: " + err.Error())
	}

	return &Node{
		Name:        name,
		peers:       make(map[string]*Peer),
		SharedFiles: make([]string, 0),
		stopChan:    make(chan struct{}),
		downloadDir: downloadDir,
		tlsConfig:   tlsConfig,
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
	stat, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	n.sharedMu.Lock()
	defer n.sharedMu.Unlock()

	if stat.IsDir() {
		// Share all files in directory recursively
		return filepath.Walk(filePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip errors
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
	tcpListener, err := net.Listen("tcp", fmt.Sprintf(":%d", TransferPort))
	if err != nil {
		return
	}
	defer tcpListener.Close()

	listener := tls.NewListener(tcpListener, n.tlsConfig)

	for n.running {
		tcpListener.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
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
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", peerAddr, &tls.Config{
		InsecureSkipVerify: true,
	})
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

	// Use MultiReader to first drain any bytes buffered by the JSON decoder,
	// then continue reading from the connection. This fixes cross-platform
	// issues where TCP packetization differences cause the decoder to buffer
	// part of the file data.
	reader := io.MultiReader(decoder.Buffered(), conn)

	buf := make([]byte, BufferSize)
	var received int64 = 0

	for received < response.FileSize {
		nr, err := reader.Read(buf)
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

func (n *Node) GetSharedFilesList(peerAddr string) ([]SharedFile, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", peerAddr, &tls.Config{
		InsecureSkipVerify: true,
	})
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
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				// Skip link-local addresses (169.254.x.x on Windows APIPA)
				if ipnet.IP.IsLinkLocalUnicast() {
					continue
				}
				return ip4.String()
			}
		}
	}
	return "127.0.0.1"
}
