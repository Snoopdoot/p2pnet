package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"p2pnet/peer"
)

type PeerFile struct {
	File     peer.SharedFile
	PeerName string
	PeerAddr string
}

type downloadState struct {
	fileName string
	cancel   context.CancelFunc
}

type P2PApp struct {
	window           fyne.Window
	node             *peer.Node
	peerList         *widget.List
	sharedList       *widget.List
	peerFilesList    *widget.List
	peers            []*peer.Peer
	allPeerFiles     []PeerFile
	filesMu          sync.RWMutex
	statusLabel      *widget.Label
	progressBar      *widget.ProgressBar
	cancelBtn        *widget.Button
	activeDownload   *downloadState
	activeDownloadMu sync.Mutex
	stopChan         chan struct{}
}

func main() {
	hostname, _ := os.Hostname()

	// Get download directory based on OS
	var downloadDir string
	if runtime.GOOS == "windows" {
		downloadDir = filepath.Join(os.Getenv("USERPROFILE"), "Downloads")
	} else {
		downloadDir = filepath.Join(os.Getenv("HOME"), "Downloads")
	}
	if _, err := os.Stat(downloadDir); os.IsNotExist(err) {
		downloadDir = os.TempDir()
	}

	a := app.NewWithID("com.p2pnet.app")
	a.Settings().SetTheme(theme.DarkTheme())

	w := a.NewWindow("P2PNet - File Sharing")
	w.Resize(fyne.NewSize(900, 600))

	node := peer.NewNode(hostname, downloadDir)

	p2pApp := &P2PApp{
		window:       w,
		node:         node,
		peers:        make([]*peer.Peer, 0),
		allPeerFiles: make([]PeerFile, 0),
		stopChan:     make(chan struct{}),
	}

	p2pApp.setupUI()
	p2pApp.setupCallbacks()

	if err := node.Start(); err != nil {
		dialog.ShowError(err, w)
	}

	go p2pApp.autoRefresh()

	w.SetOnClosed(func() {
		close(p2pApp.stopChan)
		node.Stop()
	})

	w.ShowAndRun()
}

func (p *P2PApp) setupUI() {
	p.statusLabel = widget.NewLabel("Starting...")
	p.progressBar = widget.NewProgressBar()
	p.progressBar.Hide()
	p.cancelBtn = widget.NewButton("Cancel", func() {
		p.cancelDownload()
	})
	p.cancelBtn.Hide()

	// Peer list
	p.peerList = widget.NewList(
		func() int { return len(p.peers) },
		func() fyne.CanvasObject {
			return widget.NewLabel("Peer Name")
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id < len(p.peers) {
				obj.(*widget.Label).SetText(p.peers[id].Name)
			}
		},
	)

	// All available files list (from all peers)
	p.peerFilesList = widget.NewList(
		func() int {
			p.filesMu.RLock()
			defer p.filesMu.RUnlock()
			return len(p.allPeerFiles)
		},
		func() fyne.CanvasObject {
			return widget.NewLabel("filename.ext (0 MB) - PeerName")
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			p.filesMu.RLock()
			defer p.filesMu.RUnlock()
			if id < len(p.allPeerFiles) {
				pf := p.allPeerFiles[id]
				size := formatSize(pf.File.Size)
				obj.(*widget.Label).SetText(fmt.Sprintf("%s (%s) - %s", pf.File.Name, size, pf.PeerName))
			}
		},
	)

	p.peerFilesList.OnSelected = func(id widget.ListItemID) {
		p.filesMu.RLock()
		if id >= len(p.allPeerFiles) {
			p.filesMu.RUnlock()
			return
		}
		pf := p.allPeerFiles[id]
		p.filesMu.RUnlock()

		dialog.ShowConfirm("Download File",
			fmt.Sprintf("Download '%s' (%s) from %s?", pf.File.Name, formatSize(pf.File.Size), pf.PeerName),
			func(ok bool) {
				if ok {
					go p.downloadFile(pf.PeerAddr, pf.File.Name)
				}
			},
			p.window,
		)
		p.peerFilesList.UnselectAll()
	}

	// Your shared files list
	p.sharedList = widget.NewList(
		func() int { return len(p.node.SharedFiles) },
		func() fyne.CanvasObject {
			return widget.NewLabel("File Name")
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id < len(p.node.SharedFiles) {
				obj.(*widget.Label).SetText(filepath.Base(p.node.SharedFiles[id]))
			}
		},
	)

	p.sharedList.OnSelected = func(id widget.ListItemID) {
		if id < len(p.node.SharedFiles) {
			filePath := p.node.SharedFiles[id]
			fileName := filepath.Base(filePath)
			dialog.ShowConfirm("Stop Sharing",
				fmt.Sprintf("Stop sharing '%s'?", fileName),
				func(ok bool) {
					if ok {
						p.node.UnshareFile(filePath)
						p.sharedList.Refresh()
						p.statusLabel.SetText(fmt.Sprintf("Stopped sharing: %s", fileName))
					}
				},
				p.window,
			)
		}
		p.sharedList.UnselectAll()
	}

	// Buttons
	addFileBtn := widget.NewButton("+ Share File", func() {
		dialog.ShowFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			reader.Close()
			filePath := reader.URI().Path()
			if err := p.node.ShareFile(filePath); err != nil {
				dialog.ShowError(err, p.window)
				return
			}
			p.sharedList.Refresh()
			p.statusLabel.SetText(fmt.Sprintf("Now sharing: %s", filepath.Base(filePath)))
		}, p.window)
	})

	addFolderBtn := widget.NewButton("+ Share Folder", func() {
		dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
			if err != nil || uri == nil {
				return
			}
			folderPath := uri.Path()
			folderName := filepath.Base(folderPath)

			fyne.Do(func() {
				p.progressBar.Show()
				p.progressBar.SetValue(0)
				p.statusLabel.SetText(fmt.Sprintf("Zipping folder: %s...", folderName))
			})

			go func() {
				err := p.node.ShareFileWithProgress(folderPath, func(progress float64) {
					fyne.Do(func() {
						p.progressBar.SetValue(progress)
					})
				})
				fyne.Do(func() {
					p.progressBar.Hide()
					if err != nil {
						p.statusLabel.SetText(fmt.Sprintf("Error: %v", err))
						dialog.ShowError(err, p.window)
						return
					}
					p.sharedList.Refresh()
					p.statusLabel.SetText(fmt.Sprintf("Now sharing: %s.zip", folderName))
				})
			}()
		}, p.window)
	})

	refreshBtn := widget.NewButton("Refresh", func() {
		go p.refreshAllPeerFiles()
	})

	// Layout - 3 columns
	peersCard := widget.NewCard("Peers", "On your network", p.peerList)
	peerFilesCard := widget.NewCard("All Available Files", "Click to download (auto-refreshes)", p.peerFilesList)
	sharedCard := widget.NewCard("Your Shared Files", "Visible to peers", p.sharedList)

	leftPanel := container.NewBorder(nil, nil, nil, nil, peersCard)
	middlePanel := container.NewBorder(nil, refreshBtn, nil, nil, peerFilesCard)
	shareButtons := container.NewVBox(addFileBtn, addFolderBtn)
	rightPanel := container.NewBorder(nil, shareButtons, nil, nil, sharedCard)

	split1 := container.NewHSplit(leftPanel, middlePanel)
	split1.SetOffset(0.33)

	split2 := container.NewHSplit(split1, rightPanel)
	split2.SetOffset(0.66)

	topBar := container.NewHBox(
		widget.NewLabel("P2PNet"),
	)

	progressRow := container.NewBorder(nil, nil, nil, p.cancelBtn, p.progressBar)
	statusBar := container.NewVBox(
		progressRow,
		p.statusLabel,
	)

	content := container.NewBorder(topBar, statusBar, nil, nil, split2)
	p.window.SetContent(content)
	p.statusLabel.SetText("Ready - Scanning for peers...")
}

func (p *P2PApp) refreshAllPeerFiles() {
	peers := p.node.GetPeers()
	var allFiles []PeerFile

	for _, peer := range peers {
		files, err := p.node.GetSharedFilesList(peer.Addr)
		if err != nil {
			continue
		}
		for _, f := range files {
			allFiles = append(allFiles, PeerFile{
				File:     f,
				PeerName: peer.Name,
				PeerAddr: peer.Addr,
			})
		}
	}

	// Sort by peer name, then by filename for stable ordering
	sort.Slice(allFiles, func(i, j int) bool {
		if allFiles[i].PeerName != allFiles[j].PeerName {
			return allFiles[i].PeerName < allFiles[j].PeerName
		}
		return allFiles[i].File.Name < allFiles[j].File.Name
	})

	p.filesMu.Lock()
	p.allPeerFiles = allFiles
	p.filesMu.Unlock()

	fyne.Do(func() {
		p.peerFilesList.Refresh()
		p.statusLabel.SetText(fmt.Sprintf("Found %d file(s) from %d peer(s)", len(allFiles), len(peers)))
	})
}

func (p *P2PApp) autoRefresh() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.refreshAllPeerFiles()
		}
	}
}

func (p *P2PApp) downloadFile(peerAddr string, fileName string) {
	ctx, cancel := context.WithCancel(context.Background())

	p.activeDownloadMu.Lock()
	p.activeDownload = &downloadState{
		fileName: fileName,
		cancel:   cancel,
	}
	p.activeDownloadMu.Unlock()

	fyne.Do(func() {
		p.progressBar.Show()
		p.progressBar.SetValue(0)
		p.cancelBtn.Show()
		p.statusLabel.SetText(fmt.Sprintf("Downloading: %s", fileName))
	})

	err := p.node.RequestFile(ctx, peerAddr, fileName)

	p.activeDownloadMu.Lock()
	p.activeDownload = nil
	p.activeDownloadMu.Unlock()

	fyne.Do(func() {
		p.progressBar.Hide()
		p.cancelBtn.Hide()
		if err != nil {
			if err == context.Canceled {
				p.statusLabel.SetText(fmt.Sprintf("Cancelled: %s", fileName))
			} else {
				p.statusLabel.SetText(fmt.Sprintf("Error: %v", err))
			}
		}
	})
}

func (p *P2PApp) cancelDownload() {
	p.activeDownloadMu.Lock()
	defer p.activeDownloadMu.Unlock()
	if p.activeDownload != nil {
		p.activeDownload.cancel()
	}
}

func (p *P2PApp) setupCallbacks() {
	p.node.OnPeerFound = func(foundPeer *peer.Peer) {
		fyne.Do(func() {
			p.peers = p.node.GetPeers()
			p.peerList.Refresh()
			p.statusLabel.SetText(fmt.Sprintf("Found peer: %s", foundPeer.Name))
		})
		go p.refreshAllPeerFiles()
	}

	p.node.OnPeerLost = func(lostPeer *peer.Peer) {
		fyne.Do(func() {
			p.peers = p.node.GetPeers()
			p.peerList.Refresh()
			p.statusLabel.SetText(fmt.Sprintf("Lost peer: %s", lostPeer.Name))
		})
		go p.refreshAllPeerFiles()
	}

	p.node.OnTransfer = func(transfer *peer.FileTransfer) {
		fyne.Do(func() {
			p.progressBar.SetValue(transfer.Progress)
			speedStr := formatSpeed(transfer.BytesPerSecond)
			etaStr := formatDuration(transfer.ETA)
			p.statusLabel.SetText(fmt.Sprintf("Downloading: %s - %s - %s remaining",
				transfer.FileName, speedStr, etaStr))
		})
	}

	p.node.OnFileReceived = func(filePath string) {
		fyne.Do(func() {
			fileName := filepath.Base(filePath)
			p.statusLabel.SetText(fmt.Sprintf("Downloaded: %s", fileName))

			// Check if it's a zip file and offer to extract
			if strings.HasSuffix(strings.ToLower(fileName), ".zip") {
				folderName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
				extractDir := filepath.Join(filepath.Dir(filePath), folderName)

				dialog.ShowConfirm("Extract Archive",
					fmt.Sprintf("Extract '%s' to folder '%s'?", fileName, folderName),
					func(extract bool) {
						if extract {
							go func() {
								err := peer.UnzipFile(filePath, extractDir)
								fyne.Do(func() {
									if err != nil {
										p.statusLabel.SetText(fmt.Sprintf("Extract error: %v", err))
										dialog.ShowError(err, p.window)
									} else {
										p.statusLabel.SetText(fmt.Sprintf("Extracted to: %s", folderName))
										dialog.ShowInformation("Extraction Complete",
											fmt.Sprintf("Files extracted to:\n%s", extractDir), p.window)
									}
								})
							}()
						} else {
							dialog.ShowInformation("Download Complete",
								fmt.Sprintf("File saved to:\n%s", filePath), p.window)
						}
					},
					p.window,
				)
			} else {
				dialog.ShowInformation("Download Complete",
					fmt.Sprintf("File saved to:\n%s", filePath), p.window)
			}
		})
	}
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func formatSpeed(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "-- B/s"
	}
	const unit = 1024
	if bytesPerSecond < unit {
		return fmt.Sprintf("%.0f B/s", bytesPerSecond)
	}
	div, exp := float64(unit), 0
	for n := bytesPerSecond / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB/s", bytesPerSecond/div, "KMGTPE"[exp])
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "--"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}
