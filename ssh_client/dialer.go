package ssh_client

import "golang.org/x/crypto/ssh"

type Dialer interface {
	Dial(network, addr string, config *ssh.ClientConfig) (SshClient, error)
}
