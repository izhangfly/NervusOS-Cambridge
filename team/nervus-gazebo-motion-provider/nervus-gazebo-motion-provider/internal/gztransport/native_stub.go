//go:build !linux || !cgo

package gztransport

func openNative(Config) (Client, error) {
	return nil, &Error{
		Status:    StatusUnavailable,
		Operation: "create adapter",
		Detail:    "requires Linux, cgo, Gazebo Transport 15 and Gazebo Msgs 12",
	}
}

func nativeVersions() (Versions, error) {
	return Versions{}, &Error{
		Status:    StatusUnavailable,
		Operation: "read native versions",
		Detail:    "requires Linux, cgo, Gazebo Transport 15 and Gazebo Msgs 12",
	}
}
