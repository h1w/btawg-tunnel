package service

// Unit is a managed btawg role.
type Unit string

const (
	Server Unit = "server"
	Client Unit = "client"

	NotInstalled = "not installed"
)
