package main

import (
	"fmt"
	"os"
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"p2pnet/peer"
)

type P2PApp struct {
	window       fyne.Window
	node         *peer.Node
	peerList     *widget.List
	sharedList   *widget.List
	peers        []*peer.Peer
	statusLabel  *widget.Label
	progressBar  *widget.ProgressBar
	selectedPeer *peer.Peer
}

func main() {
	hostname, _ := os.Hostname()
	downloadDir := filepath.Join(os.Getenv("HOME"), "Downloads")
	if _, err := os.Stat(downloadDir); os.IsNotExist(err) {
		downloadDir = os.TempDir()
	}

	a := app.NewWithID("com.p2pnet.app")
	a.Settings().SetTheme(theme.DarkTheme())

	w := a.NewWindow("P2PNet - File Sharing")
	w.Resize(fyne.NewSize(800, 600))

	node := peer.NewNode(hostname, downloadDir)

	p2pApp := &P2PApp{
		window: w,
		node:   node,
		peers:  make([]*peer.Peer, 0),
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
	// Status bar
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
				obj.(*widget.Label).SetText(fmt.Sprintf("%s (%s)", p.peers[id].Name, p.peers[id].Addr))
			}
		},
	)

	p.peerList.OnSelected = func(id widget.ListItemID) {
		if id < len(p.peers) {
			p.selectedPeer = p.peers[id]
			p.statusLabel.SetText(fmt.Sprintf("Selected: %s", p.selectedPeer.Name))
		}
	}

	// Shared files list
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

	// Buttons
	addFileBtn := widget.NewButton("Share File", func() {
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
			p.statusLabel.SetText(fmt.Sprintf("Sharing: %s", filepath.Base(filePath)))
		}, p.window)
	})

	removeFileBtn := widget.NewButton("Unshare", func() {
		if len(p.node.SharedFiles) == 0 {
			return
		}
		// Remove last shared file for simplicity
		if len(p.node.SharedFiles) > 0 {
			p.node.UnshareFile(p.node.SharedFiles[len(p.node.SharedFiles)-1])
			p.sharedList.Refresh()
		}
	})

	requestFileBtn := widget.NewButton("Request File", func() {
		if p.selectedPeer == nil {
			dialog.ShowInformation("No Peer", "Select a peer first", p.window)
			return
		}

		entry := widget.NewEntry()
		entry.SetPlaceHolder("Enter filename to request")

		dialog.ShowForm("Request File", "Download", "Cancel",
			[]*widget.FormItem{
				widget.NewFormItem("Filename", entry),
			},
			func(ok bool) {
				if !ok || entry.Text == "" {
					return
				}
				go func() {
					p.progressBar.Show()
					p.progressBar.SetValue(0)
					p.statusLabel.SetText(fmt.Sprintf("Downloading: %s", entry.Text))

					err := p.node.RequestFile(p.selectedPeer.Addr, entry.Text)
					if err != nil {
						p.statusLabel.SetText(fmt.Sprintf("Error: %v", err))
					}
					p.progressBar.Hide()
				}()
			},
			p.window,
		)
	})

	// Layout
	peersCard := widget.NewCard("Peers", "Discovered on network", p.peerList)
	sharedCard := widget.NewCard("Shared Files", "Your shared files", p.sharedList)

	leftPanel := container.NewBorder(nil, nil, nil, nil, peersCard)
	rightPanel := container.NewBorder(
		nil,
		container.NewVBox(addFileBtn, removeFileBtn),
		nil, nil,
		sharedCard,
	)

	split := container.NewHSplit(leftPanel, rightPanel)
	split.SetOffset(0.5)

	topBar := container.NewHBox(
		widget.NewLabel("P2PNet"),
		widget.NewSeparator(),
		requestFileBtn,
	)

	statusBar := container.NewVBox(
		p.progressBar,
		p.statusLabel,
	)

	content := container.NewBorder(topBar, statusBar, nil, nil, split)
	p.window.SetContent(content)
	p.statusLabel.SetText("Ready - Scanning for peers...")
}

func (p *P2PApp) setupCallbacks() {
	p.node.OnPeerFound = func(peer *peer.Peer) {
		p.peers = p.node.GetPeers()
		p.peerList.Refresh()
		p.statusLabel.SetText(fmt.Sprintf("Found peer: %s", peer.Name))
	}

	p.node.OnPeerLost = func(peer *peer.Peer) {
		p.peers = p.node.GetPeers()
		p.peerList.Refresh()
		if p.selectedPeer != nil && p.selectedPeer.Addr == peer.Addr {
			p.selectedPeer = nil
		}
		p.statusLabel.SetText(fmt.Sprintf("Lost peer: %s", peer.Name))
	}

	p.node.OnTransfer = func(transfer *peer.FileTransfer) {
		p.progressBar.SetValue(transfer.Progress)
	}

	p.node.OnFileReceived = func(filePath string) {
		p.statusLabel.SetText(fmt.Sprintf("Downloaded: %s", filepath.Base(filePath)))
		dialog.ShowInformation("Download Complete", fmt.Sprintf("File saved to: %s", filePath), p.window)
	}
}
