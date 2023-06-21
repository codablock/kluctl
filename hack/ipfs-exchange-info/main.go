package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"filippo.io/age"
	"flag"
	"fmt"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	log "github.com/sirupsen/logrus"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/kubo/client/rpc"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
	mh "github.com/multiformats/go-multihash"
)

var modeFlag string
var topicFlag string
var staticIpnsName string
var prNumber int
var ageKeyFile string
var agePubKey string
var repoName string
var outFile string

func ParseFlags() error {
	flag.StringVar(&modeFlag, "mode", "", "Mode")
	flag.StringVar(&topicFlag, "topic", "", "pubsub topic")
	flag.StringVar(&staticIpnsName, "static-ipns-name", "", "Static Webui IPNS name")
	flag.IntVar(&prNumber, "pr-number", 0, "PR number")
	flag.StringVar(&ageKeyFile, "age-key-file", "", "AGE key file")
	flag.StringVar(&agePubKey, "age-pub-key", "", "AGE pubkey")
	flag.StringVar(&repoName, "repo-name", "", "Repo name")
	flag.StringVar(&outFile, "out-file", "", "Output file")
	flag.Parse()

	return nil
}

func main() {
	//logging.SetLogLevel("*", "INFO")

	err := ParseFlags()
	if err != nil {
		panic(err)
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter num: ")
	text, _ := reader.ReadString('\n')
	topicFlag = "my-test-topic-" + strings.TrimSpace(text)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// "Connect" to local node
	ipfsNode, err := rpc.NewLocalApi()
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}

	var h host.Host
	h, err = libp2p.New(libp2p.NoListenAddrs)
	if err != nil {
		panic(err)
	}

	log.Infof("own ID: %s", h.ID().String())

	kademliaDHT, err := initDHT(ctx, h)
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}
	discovery := drouting.NewRoutingDiscovery(kademliaDHT)

	switch modeFlag {
	case "publish":
		err = doPublish(ctx, h, discovery, ipfsNode)
	case "subscribe":
		err = doSubscribe(ctx, h, kademliaDHT, discovery, ipfsNode)
	default:
		err = fmt.Errorf("unknown mode %s", modeFlag)
	}

	if err != nil {
		log.Error(err)
		log.Exit(1)
	} else {
		log.Exit(0)
	}
}

func initDHT(ctx context.Context, h host.Host) (*dht.IpfsDHT, error) {
	// Start a DHT, for use in peer discovery. We can't just make a new DHT
	// client because we want each peer to maintain its own local copy of the
	// DHT, so that the bootstrapping node of the DHT can go down without
	// inhibiting future peer discovery.
	kademliaDHT, err := dht.New(ctx, h)
	if err != nil {
		return nil, err
	}
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	for _, peerAddr := range dht.DefaultBootstrapPeers {
		peerinfo, _ := peer.AddrInfoFromP2pAddr(peerAddr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Connect(ctx, *peerinfo); err != nil {
				log.Info("Bootstrap warning:", err)
			} else {
				log.Infof("Connected to bootstrap peer: %s", peerinfo.String())
			}
		}()
	}
	wg.Wait()

	return kademliaDHT, nil
}

type workflowInfo struct {
	PrNumber       int    `json:"prNumber"`
	IpfsId         string `json:"ipfsId"`
	StaticIpnsName string `json:"staticIpnsName"`
	GithubToken    string `json:"githubToken"`
}

func doPublish(ctx context.Context, h host.Host, discovery *drouting.RoutingDiscovery, ipfsNode *rpc.HttpApi) error {
	selfKey, err := ipfsNode.Key().Self(ctx)
	if err != nil {
		return err
	}

	info := workflowInfo{
		PrNumber:       prNumber,
		GithubToken:    os.Getenv("GITHUB_TOKEN"),
		IpfsId:         selfKey.ID().String(),
		StaticIpnsName: staticIpnsName,
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

	for {
		peersCh, err := discovery.FindPeers(ctx, topicFlag)
		if err != nil {
			return err
		}
		didSend := false
		for peer := range peersCh {
			if peer.ID == h.ID() {
				continue // No self connection
			}

			err = p2pSendFile(ctx, ipfsNode, peer.ID, b)
			if err != nil {
				log.Warnf("doSendInfo failed for %s: %v", peer.ID, err)
				continue
			}
			didSend = true
		}
		if didSend {
			break
		}
	}

	log.Info("Done sending info.")

	return nil
}

func doSubscribe(ctx context.Context, h host.Host, dht *dht.IpfsDHT, discovery *drouting.RoutingDiscovery, ipfsNode *rpc.HttpApi) error {
	doneCh := make(chan bool)
	h.SetStreamHandler("/x/kluctl-preview-info", func(s network.Stream) {
		defer s.Close()

		enc := gob.NewEncoder(s)
		dec := gob.NewDecoder(s)

		var b []byte
		err := dec.Decode(&b)
		if err != nil {
			log.Infof("Receive failed: %v", err)
			return
		}

		err = handleInfo(ctx, b)
		if err != nil {
			log.Infof("handle failed: %v", err)
			return
		}

		err = enc.Encode("ok")
		if err != nil {
			log.Infof("Sending ok failed: %v", err)
			return
		}
		doneCh <- true
	})

	/*hash, err := nsToCid(topicFlag)
	if err != nil {
		return err
	}

	dht.ProviderStore().AddProvider(ctx, hash.Hash())*/
	dutil.Advertise(ctx, discovery, topicFlag)
	<-doneCh
	return nil
}

func nsToCid(ns string) (cid.Cid, error) {
	h, err := mh.Sum([]byte(ns), mh.SHA2_256, -1)
	if err != nil {
		return cid.Undef, err
	}

	return cid.NewCidV1(cid.Raw, h), nil
}

func handleInfo(ctx context.Context, data []byte) error {
	idsBytes, err := os.ReadFile(ageKeyFile)
	if err != nil {
		return err
	}
	ageIds, err := age.ParseIdentities(bytes.NewReader(idsBytes))
	if err != nil {
		return err
	}
	d, err := age.Decrypt(bytes.NewReader(data), ageIds...)
	if err != nil {
		return err
	}

	w := bytes.NewBuffer(nil)
	_, err = io.Copy(w, d)
	if err != nil {
		return err
	}
	data = w.Bytes()

	var info workflowInfo
	err = json.Unmarshal(data, &info)
	if err != nil {
		return err
	}

	if info.PrNumber != prNumber {
		return fmt.Errorf("%d is not the expected (%d) PR number", info.PrNumber, prNumber)
	}

	log.Info("Checking Github token...")

	err = checkGithubToken(ctx, info.GithubToken)
	if err != nil {
		return err
	}

	log.Info("Done checking Github token...")

	info.GithubToken = ""

	data, err = json.Marshal(&info)
	if err != nil {
		return err
	}
	err = os.WriteFile(outFile, data, 0o600)
	if err != nil {
		return err
	}
	return nil
}

func doGithubRequest(ctx context.Context, method string, url string, body string, token string) ([]byte, error) {
	log.Infof("request: %s %s", method, url)

	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		log.Error("NewRequest failed: ", err)
		return nil, err
	}
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("token %s", token))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error("Request failed: ", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Error(fmt.Sprintf("Request failed: %d - %v", resp.StatusCode, resp.Status))
		return nil, fmt.Errorf("http error: %s", resp.Status)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error("Failed to read body: ", err)
		return nil, err
	}

	return b, nil
}

func checkGithubToken(ctx context.Context, token string) error {
	body := fmt.Sprintf(`{"query": "query UserCurrent{viewer{login}}"}`)
	b, err := doGithubRequest(ctx, "POST", "https://api.github.com/graphql", body, token)
	if err != nil {
		return err
	}
	log.Info("body=", string(b))

	var r struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
		} `json:"data"`
	}
	err = json.Unmarshal(b, &r)
	if err != nil {
		log.Error("Unmarshal failed: ", err)
		return err
	}
	if r.Data.Viewer.Login != "github-actions[bot]" {
		log.Error("unexpected response from github")
		return fmt.Errorf("unexpected response from github")
	}

	log.Info("Querying repositories...")

	b, err = doGithubRequest(ctx, "GET", "https://api.github.com/installation/repositories", "", token)
	if err != nil {
		return err
	}
	log.Info("body=", string(b))

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

func p2pSendFile(ctx context.Context, node *rpc.HttpApi, ipfsId peer.ID, data []byte) error {
	// close the old one
	_, _ = node.Request("p2p/close").
		Option("protocol", "/x/kluctl-preview-info").
		Option("listen-address", "/ip4/127.0.0.1/tcp/10001").
		Send(ctx)

	_, err := node.Request("p2p/forward", "/x/kluctl-preview-info", "/ip4/127.0.0.1/tcp/10001", fmt.Sprintf("/p2p/%s", ipfsId)).
		Send(ctx)
	if err != nil {
		return err
	}

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

func p2pReceiveFiles(ctx context.Context, node *rpc.HttpApi, cb func([]byte) error) error {
	l, err := net.Listen("tcp", "127.0.0.1:10002")
	if err != nil {
		return err
	}
	defer l.Close()
	addr := l.Addr().(*net.TCPAddr)

	targetAddr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d", addr.Port)

	// close the old one
	_, _ = node.Request("p2p/close").
		Option("protocol", "/x/kluctl-preview-info").
		Option("target-address", targetAddr).
		Send(ctx)
	_, err = node.Request("p2p/listen", "/x/kluctl-preview-info", targetAddr).
		Send(ctx)
	if err != nil {
		return err
	}

	handleConn := func(c net.Conn) error {
		defer c.Close()

		d := gob.NewDecoder(c)
		e := gob.NewEncoder(c)

		var data []byte
		err = d.Decode(&data)
		if err != nil {
			return err
		}

		err = cb(data)
		if err != nil {
			return err
		}

		ok := "ok"
		err = e.Encode(&ok)
		if err != nil {
			return err
		}
		return nil
	}

	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		err = handleConn(c)
		if err != nil {
			log.Infof("handleConn failed: %v", err)
			continue
		}
		break
	}

	return nil
}
