package auth

import (
	"fmt"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/kevinburke/ssh_config"
	git_url "github.com/kluctl/kluctl/pkg/git/git-url"
	"github.com/kluctl/kluctl/pkg/utils"
	log "github.com/sirupsen/logrus"
	sshagent "github.com/xanzy/ssh-agent"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
)

type GitSshAuthProvider struct {
}

type sshDefaultIdentityAndAgent struct {
	hostname        string
	user            string
	defaultIdentity ssh.Signer
	agent           agent.Agent
}

func (a *sshDefaultIdentityAndAgent) String() string {
	return fmt.Sprintf("user: %s, name: %s", a.user, a.Name())
}

func (a *sshDefaultIdentityAndAgent) Name() string {
	return "ssh-default-identity-and-agent"
}

func (a *sshDefaultIdentityAndAgent) ClientConfig() (*ssh.ClientConfig, error) {
	cc := &ssh.ClientConfig{
		User: a.user,
		Auth: []ssh.AuthMethod{ssh.PublicKeysCallback(a.Signers)},
	}
	cc.HostKeyCallback = verifyHost
	return cc, nil
}

func (a *sshDefaultIdentityAndAgent) Signers() ([]ssh.Signer, error) {
	var ret []ssh.Signer
	for _, id := range ssh_config.GetAll(a.hostname, "IdentityFile") {
		id = utils.ExpandPath(id)
		signer, err := readKey(id)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			ret = append(ret, signer)
		}
	}
	if a.defaultIdentity != nil {
		ret = append(ret, a.defaultIdentity)
	}
	if a.agent != nil {
		s, err := a.agent.Signers()
		if err != nil {
			return nil, err
		}
		ret = append(ret, s...)
	}
	return ret, nil
}

func (a *GitSshAuthProvider) BuildAuth(gitUrl git_url.GitUrl) transport.AuthMethod {
	if !gitUrl.IsSsh() {
		return nil
	}
	if gitUrl.User == nil {
		return nil
	}

	auth := &sshDefaultIdentityAndAgent{
		hostname: gitUrl.Hostname(),
		user:     gitUrl.User.Username(),
	}

	u, err := user.Current()
	if err != nil {
		log.Debugf("No current user: %v", err)
	} else {
		signer, err := readKey(filepath.Join(u.HomeDir, ".ssh", "id_rsa"))
		if err != nil {
			log.Debugf("Failed to read default identity file for url %s: %v", gitUrl.String(), err)
		} else if signer != nil {
			auth.defaultIdentity = signer
		}
	}

	agent, _, err := sshagent.New()
	if err != nil {
		log.Debugf("Failed to connect to ssh agent for url %s: %v", gitUrl.String(), err)
	} else {
		auth.agent = agent
	}

	return auth
}

func readKey(path string) (ssh.Signer, error) {
	pemBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	} else {
		signer, err := ssh.ParsePrivateKey(pemBytes)
		if err != nil {
			return nil, err
		}
		return signer, nil
	}
}
