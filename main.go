package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"p2pnet/peer"
)

type P2PApp struct {
	window        fyne.Window
	node          *peer.Node
	peerList      *widget.List
	sharedList    *widget.List
	peerFilesList *widget.List
	peers         []*peer.Peer
	peerFiles     []peer.SharedFile
	statusLabel   *widget.Label
	progressBar   *widget.ProgressBar
	selectedPeer  *peer.Peer
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
		window:    w,
		node:      node,
		peers:     make([]*peer.Peer, 0),
		peerFiles: make([]peer.SharedFile, 0),
	}

	p2pApp.setupUI()
	p2pApp.setupCallbacks()

	if err := node.Start(); err != nil {
		dialog.ShowError(err, w)
	}

	w.SetOnClosed(func() {
		node.Stop()
	})

	w.ShowAndRun()
}

func (p *P2PApp) setupUI() {
	p.statusLabel = widget.NewLabel("Starting...")
	p.progressBar = widget.NewProgressBar()
	p.progressBar.Hide()

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

	p.peerList.OnSelected = func(id widget.ListItemID) {
		if id < len(p.peers) {
			p.selectedPeer = p.peers[id]
			p.statusLabel.SetText(fmt.Sprintf("Loading files from %s...", p.selectedPeer.Name))
			go p.loadPeerFiles()
		}
	}

	// Peer's available files list
	p.peerFilesList = widget.NewList(
		func() int { return len(p.peerFiles) },
		func() fyne.CanvasObject {
			return widget.NewLabel("filename.ext (0 MB)")
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id < len(p.peerFiles) {
				f := p.peerFiles[id]
				size := formatSize(f.Size)
				obj.(*widget.Label).SetText(fmt.Sprintf("%s (%s)", f.Name, size))
			}
		},
	)

	p.peerFilesList.OnSelected = func(id widget.ListItemID) {
		if id < len(p.peerFiles) && p.selectedPeer != nil {
			file := p.peerFiles[id]
			dialog.ShowConfirm("Download File",
				fmt.Sprintf("Download '%s' (%s)?", file.Name, formatSize(file.Size)),
				func(ok bool) {
					if ok {
						go p.downloadFile(file.Name)
					}
				},
				p.window,
			)
		}
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
			if err := p.node.ShareFile(folderPath); err != nil {
				dialog.ShowError(err, p.window)
				return
			}
			p.sharedList.Refresh()
			p.statusLabel.SetText(fmt.Sprintf("Now sharing folder: %s (%d files)", filepath.Base(folderPath), len(p.node.SharedFiles)))
		}, p.window)
	})

	refreshBtn := widget.NewButton("Refresh", func() {
		if p.selectedPeer != nil {
			go p.loadPeerFiles()
		}
	})

	// Layout - 3 columns
	peersCard := widget.NewCard("Peers", "On your network", p.peerList)
	peerFilesCard := widget.NewCard("Available Files", "Click to download", p.peerFilesList)
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

	statusBar := container.NewVBox(
		p.progressBar,
		p.statusLabel,
	)

	content := container.NewBorder(topBar, statusBar, nil, nil, split2)
	p.window.SetContent(content)
	p.statusLabel.SetText("Ready - Scanning for peers...")
}

func (p *P2PApp) loadPeerFiles() {
	if p.selectedPeer == nil {
		return
	}

	files, err := p.node.GetSharedFilesList(p.selectedPeer.Addr)
	if err != nil {
		fyne.Do(func() {
			p.statusLabel.SetText(fmt.Sprintf("Error: %v", err))
			p.peerFiles = []peer.SharedFile{}
			p.peerFilesList.Refresh()
		})
		return
	}

	fyne.Do(func() {
		p.peerFiles = files
		p.peerFilesList.Refresh()

		if len(files) == 0 {
			p.statusLabel.SetText(fmt.Sprintf("%s has no shared files", p.selectedPeer.Name))
		} else {
			p.statusLabel.SetText(fmt.Sprintf("%s is sharing %d file(s)", p.selectedPeer.Name, len(files)))
		}
	})
}

func (p *P2PApp) downloadFile(fileName string) {
	if p.selectedPeer == nil {
		return
	}

	fyne.Do(func() {
		p.progressBar.Show()
		p.progressBar.SetValue(0)
		p.statusLabel.SetText(fmt.Sprintf("Downloading: %s", fileName))
	})

	err := p.node.RequestFile(p.selectedPeer.Addr, fileName)
	fyne.Do(func() {
		if err != nil {
			p.statusLabel.SetText(fmt.Sprintf("Error: %v", err))
		}
		p.progressBar.Hide()
	})
}

func (p *P2PApp) setupCallbacks() {
	p.node.OnPeerFound = func(foundPeer *peer.Peer) {
		fyne.Do(func() {
			p.peers = p.node.GetPeers()
			p.peerList.Refresh()
			p.statusLabel.SetText(fmt.Sprintf("Found peer: %s", foundPeer.Name))
		})
	}

	p.node.OnPeerLost = func(lostPeer *peer.Peer) {
		fyne.Do(func() {
			p.peers = p.node.GetPeers()
			p.peerList.Refresh()
			if p.selectedPeer != nil && p.selectedPeer.Addr == lostPeer.Addr {
				p.selectedPeer = nil
				p.peerFiles = []peer.SharedFile{}
				p.peerFilesList.Refresh()
			}
			p.statusLabel.SetText(fmt.Sprintf("Lost peer: %s", lostPeer.Name))
		})
	}

	p.node.OnTransfer = func(transfer *peer.FileTransfer) {
		fyne.Do(func() {
			p.progressBar.SetValue(transfer.Progress)
		})
	}

	p.node.OnFileReceived = func(filePath string) {
		fyne.Do(func() {
			p.statusLabel.SetText(fmt.Sprintf("Downloaded: %s", filepath.Base(filePath)))
			dialog.ShowInformation("Download Complete", fmt.Sprintf("File saved to:\n%s", filePath), p.window)
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
