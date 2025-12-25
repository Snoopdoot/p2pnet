.PHONY: build linux windows clean run

build: linux

linux:
	go build -o p2pnet .

windows:
	CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui" -o p2pnet.exe .

clean:
	rm -f p2pnet p2pnet.exe

run: linux
	./p2pnet

# Install mingw on Arch for Windows builds:
# sudo pacman -S mingw-w64-gcc
