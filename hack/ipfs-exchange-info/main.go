package main

import (
	"bytes"
	"context"
	"encoding/json"
	"filippo.io/age"
	"flag"
	"fmt"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/kubo/client/rpc"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	drouting "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	dutil "github.com/libp2p/go-libp2p/p2p/discovery/util"
)

var modeFlag string
var topicFlag string
var staticIpnsName string
var prNumber int
var ageKeyFile string
var agePubKey string
var repoName string

func ParseFlags() error {
	flag.StringVar(&modeFlag, "mode", "", "Mode")
	flag.StringVar(&topicFlag, "topic", "", "pubsub topic")
	flag.StringVar(&staticIpnsName, "static-ipns-name", "", "Static Webui IPNS name")
	flag.IntVar(&prNumber, "pr-number", 0, "PR number")
	flag.StringVar(&ageKeyFile, "age-key-file", "", "AGE key file")
	flag.StringVar(&agePubKey, "age-pub-key", "", "AGE pubkey")
	flag.StringVar(&repoName, "repo-name", "", "Repo name")
	flag.Parse()

	return nil
}

const limiterCfg = `
{
  "System":  {
    "StreamsInbound": 4096,
    "StreamsOutbound": 32768,
    "Conns": 64000,
    "ConnsInbound": 512,
    "ConnsOutbound": 32768,
    "FD": 64000
  },
  "Transient": {
    "StreamsInbound": 4096,
    "StreamsOutbound": 32768,
    "ConnsInbound": 512,
    "ConnsOutbound": 32768,
    "FD": 64000
  },

  "ProtocolDefault":{
    "StreamsInbound": 1024,
    "StreamsOutbound": 32768
  },

  "ServiceDefault":{
    "StreamsInbound": 2048,
    "StreamsOutbound": 32768
  }
}
`

func main() {
	//logging.SetLogLevel("*", "INFO")

	err := ParseFlags()
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// "Connect" to local node
	ipfsNode, err := rpc.NewLocalApi()
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}

	limiter, err := rcmgr.NewDefaultLimiterFromJSON(strings.NewReader(limiterCfg))
	if err != nil {
		panic(err)
	}
	rcm, err := rcmgr.NewResourceManager(limiter)
	if err != nil {
		panic(err)
	}

	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"), libp2p.ResourceManager(rcm))
	if err != nil {
		panic(err)
	}

	log.Infof("own ID: %s", h.ID().String())

	discoverCh := make(chan error)
	go func() {
		err := discoverPeers(ctx, h)
		discoverCh <- err
	}()

	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}
	topic, err := ps.Join(topicFlag)
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}

	err = <-discoverCh
	if err != nil {
		log.Error(err)
		log.Exit(1)
	}

	switch modeFlag {
	case "publish":
		err = doPublish(ctx, topic, ipfsNode)
	case "subscribe":
		err = doSubscribe(ctx, topic, ipfsNode)
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

func discoverPeers(ctx context.Context, h host.Host) error {
	kademliaDHT, err := initDHT(ctx, h)
	if err != nil {
		return err
	}
	routingDiscovery := drouting.NewRoutingDiscovery(kademliaDHT)
	dutil.Advertise(ctx, routingDiscovery, topicFlag)

	// Look for others who have announced and attempt to connect to them
	anyConnected := false
	for !anyConnected {
		log.Info("Searching for peers...")
		peerChan, err := routingDiscovery.FindPeers(ctx, topicFlag)
		if err != nil {
			panic(err)
		}
		for peer := range peerChan {
			if peer.ID == h.ID() {
				continue // No self connection
			}
			err := h.Connect(network.WithForceDirectDial(ctx, "test"), peer)
			if err != nil {
				log.Info("Failed connecting to ", peer.ID.Pretty(), ", error:", err)
			} else {
				log.Info("Connected to:", peer.ID.Pretty())
				anyConnected = true
			}
		}
		if !anyConnected {
			time.Sleep(5 * time.Second)
		}
	}
	log.Info("Peer discovery complete")
	return nil
}

type workflowInfo struct {
	PrNumber       int    `json:"prNumber"`
	IpfsId         string `json:"ipfsId"`
	StaticIpnsName string `json:"staticIpnsName"`
	GithubToken    string `json:"githubToken"`
}

func doPublish(ctx context.Context, topic *pubsub.Topic, ipfsNode *rpc.HttpApi) error {
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

	err = topic.Publish(ctx, b)
	if err != nil {
		return err
	}

	log.Info("Done sending info.")

	return nil
}

func doSubscribe(ctx context.Context, topic *pubsub.Topic, ipfsNode *rpc.HttpApi) error {
	sub, err := topic.Subscribe()
	if err != nil {
		return err
	}

	for {
		m, err := sub.Next(ctx)
		if err != nil {
			return err
		}
		log.Info(m.ReceivedFrom, ": ", string(m.Message.Data))
		err = handleInfo(ctx, m.Message.Data)
		if err == nil {
			break
		}
		log.Error(err)
	}
	return nil
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
	_, _ = os.Stdout.WriteString(string(data) + "\n")
	return nil
}

func doGithubRequest(ctx context.Context, method string, url string, body string, token string) ([]byte, error) {
	log.Info("request: ", method, url)

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

type tracer struct {
}

func (t *tracer) Trace(evt *holepunch.Event) {
	log.Info(evt)
}
