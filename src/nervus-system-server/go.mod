module github.com/nervus-os/nervus-system-server

go 1.25.0

require (
	github.com/klauspost/compress v1.19.1
	github.com/nervus-os/nervus-ipc v0.0.0-20260811104118-34a11e8d2c4e
)

require google.golang.org/protobuf v1.36.11

require golang.org/x/sys v0.47.0

replace github.com/nervus-os/nervus-ipc => ../nervus-ipc
