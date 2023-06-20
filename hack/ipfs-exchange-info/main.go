package main

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"filippo.io/age"
	"flag"
	"fmt"
	"github.com/ipfs/boxo/coreiface/options"
	"github.com/ipfs/boxo/files"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ipfs/kubo/client/rpc"
)

var timeout time.Duration
var modeFlag string
var ipnsKey string
var ipnsName string
var ipfsId string
var workflowRunId int
var prNumber string
var ageKeyFile string
var agePubKey string
var repoName string

func ParseFlags() error {
	flag.DurationVar(&timeout, "timeout", time.Second*10, "Timeout")
	flag.StringVar(&modeFlag, "mode", "", "Mode")
	flag.StringVar(&ipnsKey, "ipns-key", "", "IPNS key name")
	flag.StringVar(&ipnsName, "ipns-name", "", "IPNS name")
	flag.IntVar(&workflowRunId, "workflow-run-id", 0, "Workflow run id")
	flag.StringVar(&ipfsId, "ipfs-id", "", "IPFS id")
	flag.StringVar(&prNumber, "pr-number", "", "PR number")
	flag.StringVar(&ageKeyFile, "age-key-file", "", "AGE key file")
	flag.StringVar(&agePubKey, "age-pub-key", "", "AGE pubkey")
	flag.StringVar(&repoName, "repo-name", "", "Repo name")
	flag.Parse()

	return nil
}

func main() {
	err := ParseFlags()
	if err != nil {
		panic(err)
	}

	// "Connect" to local node
	node, err := rpc.NewLocalApi()
	if err != nil {
		fmt.Print(err)
		os.Exit(1)
	}

	switch modeFlag {
	case "publish-ipns":
		err = doPublishIpns(node)
	case "resolve-ipns":
		err = doResolve(node)
	case "send-info":
		err = doSend(node)
	case "receive-info":
		err = doReceive(node)
	default:
		err = fmt.Errorf("unknown mode %s", modeFlag)
	}

	if err != nil {
		fmt.Print(err)
		os.Exit(1)
	} else {
		os.Exit(0)
	}
}

type ipnsInfo struct {
	WorkflowRunId int    `json:"workflowRunId"`
	IpfsId        string `json:"ipfsId"`
}

type workflowInfo struct {
	WorkflowRunId int    `json:"workflowRunId"`
	IpfsId        string `json:"ipfsId"`
	GithubToken   string `json:"githubToken"`
	PrNumber      string `json:"prNumber"`
}

func doPublishIpns(node *rpc.HttpApi) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	selfKey, err := node.Key().Self(ctx)
	if err != nil {
		return err
	}

	info := ipnsInfo{
		WorkflowRunId: workflowRunId,
		IpfsId:        selfKey.ID().String(),
	}
	b, err := json.Marshal(&info)
	if err != nil {
		return err
	}
	log.Info("publishing: ", string(b))

	f := files.NewBytesFile(b)

	pth, err := node.Unixfs().Add(ctx, f)
	if err != nil {
		return err
	}

	log.Info("path: ", pth.String())

	ipnsEntry, err := node.Name().Publish(ctx, pth, options.Name.Key(ipnsKey), options.Name.TTL(30*time.Second))
	if err != nil {
		return err
	}

	log.Info("published as ", ipnsEntry.Name())

	return nil
}

func doResolve(node *rpc.HttpApi) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	log.Info("Resolving: ", ipnsName)

	resolved, err := node.Name().Resolve(ctx, ipnsName, options.Name.Cache(false))
	if err != nil {
		return err
	}

	log.Info("Resolved to: ", resolved.String())

	nd, err := node.Unixfs().Get(ctx, resolved)
	if err != nil {
		return err
	}
	defer nd.Close()

	f, ok := nd.(files.File)
	if !ok {
		return fmt.Errorf("%s is not a file", resolved.String())
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}

	var info ipnsInfo
	err = json.Unmarshal(b, &info)
	if err != nil {
		return err
	}

	log.Info("IPNS Info: ", string(b))

	if info.WorkflowRunId != workflowRunId {
		return fmt.Errorf("IPNS entry not up-to-date")
	}

	_, err = os.Stdout.WriteString(info.IpfsId + "\n")
	if err != nil {
		return err
	}
	return nil
}

func doSend(node *rpc.HttpApi) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	selfKey, err := node.Key().Self(ctx)
	if err != nil {
		return err
	}

	info := workflowInfo{
		WorkflowRunId: workflowRunId,
		GithubToken:   os.Getenv("GITHUB_TOKEN"),
		IpfsId:        selfKey.ID().String(),
		PrNumber:      prNumber,
	}

	b, err := json.Marshal(&info)
	if err != nil {
		return err
	}

	ageRecipient, err := age.ParseX25519Recipient(agePubKey)
	if err != nil {
		return err
	}

	w := bytes.NewBuffer(nil)
	e, err := age.Encrypt(w, ageRecipient)
	if err != nil {
		return err
	}
	_, err = e.Write(b)
	if err != nil {
		return err
	}
	err = e.Close()
	if err != nil {
		return err
	}
	b = w.Bytes()

	log.Info("Sending info...")

	err = p2pSendFile(ctx, node, ipfsId, b)
	if err != nil {
		return err
	}

	log.Info("Done sending info.")

	return nil

}

func doReceive(node *rpc.HttpApi) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	log.Info("Receiving info...")

	b, err := p2pReceiveFile(ctx, node)
	if err != nil {
		return err
	}

	log.Info("Done receiving info.")

	idsBytes, err := os.ReadFile(ageKeyFile)
	if err != nil {
		return err
	}
	ageIds, err := age.ParseIdentities(bytes.NewReader(idsBytes))
	if err != nil {
		return err
	}
	d, err := age.Decrypt(bytes.NewReader(b), ageIds...)
	if err != nil {
		return err
	}

	w := bytes.NewBuffer(nil)
	_, err = io.Copy(w, d)
	if err != nil {
		return err
	}
	b = w.Bytes()

	var info workflowInfo
	err = json.Unmarshal(b, &info)
	if err != nil {
		return err
	}

	if info.WorkflowRunId != workflowRunId {
		return fmt.Errorf("%d is not the expected (%d) workflow run id", info.WorkflowRunId, workflowRunId)
	}

	log.Info("Checking Github token...")

	err = checkGithubToken(ctx, info.GithubToken)
	if err != nil {
		return err
	}

	log.Info("Done checking Github token...")

	info.GithubToken = ""

	b, err = json.Marshal(&info)
	if err != nil {
		return err
	}
	_, _ = os.Stdout.WriteString(string(b) + "\n")

	return nil
}

func checkGithubToken(ctx context.Context, token string) error {
	body := fmt.Sprintf(`{"query": "query UserCurrent{viewer{login}}"}`)
	req, err := http.NewRequest("POST", "https://api.github.com/graphql", strings.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http error: %s", resp.Status)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var r struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}
	err = json.Unmarshal(b, &r)
	if err != nil {
		return err
	}
	if r.Data.Viewer.Login != "github-actions[bot]" {
		return fmt.Errorf("unexpected response from github")
	}

	req, err = http.NewRequest("GET", "https://api.github.com/installation/repositories", nil)
	req = req.WithContext(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http error: %s", resp.Status)
	}

	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var r2 struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	err = json.Unmarshal(b, &r2)
	if err != nil {
		return err
	}
	if len(r2.Repositories) != 1 {
		return fmt.Errorf("unexpected repositories count %d", len(r2.Repositories))
	}
	if r2.Repositories[0].FullName != repoName {
		return fmt.Errorf("%s is not the expected repo name", r2.Repositories[0].FullName)
	}

	return nil
}

func p2pSendFile(ctx context.Context, node *rpc.HttpApi, ipfsId string, data []byte) error {
	_, err := node.Request("p2p/forward", "/x/kluctl-preview-info", "/ip4/127.0.0.1/tcp/10001", fmt.Sprintf("/p2p/%s", ipfsId)).
		Send(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_, err := node.Request("p2p/close").
			Option("protocol", "/x/kluctl-preview-info").
			Option("listen-address", "/ip4/127.0.0.1/tcp/10001").
			Send(ctx)
		_ = err
	}()

	c, err := net.Dial("tcp", "127.0.0.1:10001")
	if err != nil {
		return err
	}
	defer c.Close()

	e := gob.NewEncoder(c)
	d := gob.NewDecoder(c)

	err = e.Encode(data)
	if err != nil {
		return err
	}

	var ok string
	err = d.Decode(&ok)
	if err != nil {
		return err
	}
	if ok != "ok" {
		return fmt.Errorf("did not receive ok")
	}

	return nil
}

func p2pReceiveFile(ctx context.Context, node *rpc.HttpApi) ([]byte, error) {
	l, err := net.Listen("tcp", "127.0.0.1:10002")
	if err != nil {
		return nil, err
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)

	targetAddr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", addr.Port)

	_, err = node.Request("p2p/listen", "/x/kluctl-preview-info", targetAddr).
		Send(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, err := node.Request("p2p/close").
			Option("protocol", "/x/kluctl-preview-info").
			Option("target-address", targetAddr).
			Send(ctx)
		_ = err
	}()

	c, err := l.Accept()
	if err != nil {
		return nil, err
	}
	defer c.Close()

	d := gob.NewDecoder(c)
	e := gob.NewEncoder(c)

	var data []byte
	err = d.Decode(&data)
	if err != nil {
		return nil, err
	}

	ok := "ok"
	err = e.Encode(&ok)
	if err != nil {
		return nil, err
	}

	return data, nil
}
