package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"strings"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/sputn1ck/glightning/gbitcoin"
	"github.com/sputn1ck/glightning/gelements"
	"github.com/sputn1ck/glightning/glightning"
	"github.com/sputn1ck/peerswap"
	"github.com/sputn1ck/peerswap/clightning"
	"github.com/sputn1ck/peerswap/onchain"
	"github.com/sputn1ck/peerswap/policy"
	"github.com/sputn1ck/peerswap/poll"
	"github.com/sputn1ck/peerswap/swap"
	"github.com/sputn1ck/peerswap/txwatcher"
	"github.com/sputn1ck/peerswap/wallet"
	"github.com/vulpemventures/go-elements/network"
	"go.etcd.io/bbolt"
)

var supportedAssets = []string{}

func main() {
	if err := run(); err != nil {
		log.Printf("plugin quitting, error: %s", err)
		os.Exit(1)
	}

}
func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// initialize
	lightningPlugin, initChan, err := clightning.NewClightningClient()
	if err != nil {
		return err
	}

	err = lightningPlugin.RegisterOptions()
	if err != nil {
		return err
	}

	err = lightningPlugin.RegisterMethods()
	if err != nil {
		return err
	}

	// start lightningPlugin plugin
	quitChan := make(chan interface{})
	go func() {
		err := lightningPlugin.Start()
		if err != nil {
			log.Fatal(err)
		}
		quitChan <- true
	}()
	<-initChan
	log.Printf("waiting for init finished")
	config, err := lightningPlugin.GetConfig()
	if err != nil {
		return err
	}
	err = validateConfig(config)
	if err != nil {
		return err
	}
	log.Printf("Config: Db:%s, Rpc: %s %s, network: %s", config.DbPath, config.LiquidRpcHost, config.LiquidRpcUser, config.LiquidNetworkString)
	// setup services

	// liquid
	var liquidOnChainService *onchain.LiquidOnChain
	var liquidTxWatcher *txwatcher.BlockchainRpcTxWatcher
	var liquidRpcWallet *wallet.ElementsRpcWallet
	var liquidCli *gelements.Elements
	if config.LiquidEnabled {
		supportedAssets = append(supportedAssets, "l-btc")
		log.Printf("Liquid swaps enabled")
		// blockchaincli
		liquidCli = gelements.NewElements(config.LiquidRpcUser, config.LiquidRpcPassword)
		err = liquidCli.StartUp(config.LiquidRpcHost, config.LiquidRpcPort)
		if err != nil {
			return err
		}
		// Wallet
		liquidWalletCli := gelements.NewElements(config.LiquidRpcUser, config.LiquidRpcPassword)
		err = liquidWalletCli.StartUp(config.LiquidRpcHost, config.LiquidRpcPort)
		if err != nil {
			return err
		}
		liquidRpcWallet, err = wallet.NewRpcWallet(liquidWalletCli, config.LiquidRpcWallet)
		if err != nil {
			return err
		}

		// txwatcher
		liquidTxWatcher = txwatcher.NewBlockchainRpcTxWatcher(ctx, txwatcher.NewElementsCli(liquidCli), 2)

		// LiquidChain
		liquidOnChainService = onchain.NewLiquidOnChain(liquidCli, liquidTxWatcher, liquidRpcWallet, config.LiquidNetwork)
	} else {
		log.Printf("Liquid swaps disabled")
	}

	// bitcoin
	chain, err := getBitcoinChain(lightningPlugin.GetLightningRpc())
	if err != nil {
		return err
	}
	bitcoinCli, err := getBitcoinClient(lightningPlugin.GetLightningRpc())
	if err != nil {
		return err
	}
	var bitcoinTxWatcher *txwatcher.BlockchainRpcTxWatcher
	var bitcoinOnChainService *onchain.BitcoinOnChain
	var bitcoinEnabled bool
	if bitcoinCli != nil {
		supportedAssets = append(supportedAssets, "btc")
		log.Printf("Bitcoin swaps enabled")
		bitcoinEnabled = true
		bitcoinTxWatcher = txwatcher.NewBlockchainRpcTxWatcher(ctx, txwatcher.NewBitcoinRpc(bitcoinCli), 3)
		bitcoinOnChainService = onchain.NewBitcoinOnChain(bitcoinCli, bitcoinTxWatcher, lightningPlugin.GetLightningRpc(), chain)
	} else {
		log.Printf("Bitcoin swaps disabled")
	}

	if !bitcoinEnabled && !config.LiquidEnabled {
		return errors.New("bad config, either liquid or bitcoin settings must be set")
	}

	// db
	swapDb, err := bbolt.Open(filepath.Join(config.DbPath, "swaps"), 0700, nil)
	if err != nil {
		return err
	}

	// policy
	pol, err := policy.CreateFromFile(config.PolicyPath)
	if err != nil {
		return err
	}
	log.Printf("using policy:\n%s", pol)

	swapStore, err := swap.NewBboltStore(swapDb)
	if err != nil {
		return err
	}
	requestedSwapStore, err := swap.NewRequestedSwapsStore(swapDb)
	if err != nil {
		return err
	}
	swapService := swap.NewSwapService(swapStore,
		requestedSwapStore,
		config.LiquidEnabled,
		liquidOnChainService,
		bitcoinEnabled,
		bitcoinOnChainService,
		lightningPlugin,
		lightningPlugin,
		pol)

	if liquidTxWatcher != nil {
		go func() {
			err := liquidTxWatcher.StartWatchingTxs()
			if err != nil {
				log.Printf("%v", err)
				os.Exit(1)
			}
		}()
	}
	if bitcoinTxWatcher != nil {
		go func() {
			err := bitcoinTxWatcher.StartWatchingTxs()
			if err != nil {
				log.Printf("%v", err)
				os.Exit(1)
			}
		}()
	}

	err = swapService.Start()
	if err != nil {
		return err
	}
	err = swapService.RecoverSwaps()
	if err != nil {
		return err
	}

	pollStore, err := poll.NewStore(swapDb)
	if err != nil {
		return err
	}
	pollService := poll.NewService(1*time.Hour, 2*time.Hour, pollStore, lightningPlugin, pol, lightningPlugin, supportedAssets)
	pollService.Start()
	defer pollService.Stop()

	sp := swap.NewRequestedSwapsPrinter(requestedSwapStore)
	lightningPlugin.SetupClients(liquidRpcWallet, swapService, sp, pol, liquidCli, pollService)

	log.Printf("peerswap initialized")
	<-quitChan
	return nil
}

func validateConfig(cfg *peerswap.Config) error {
	if cfg.LiquidRpcUser == "" {
		cfg.LiquidEnabled = false
	} else {
		cfg.LiquidEnabled = true
	}
	if cfg.LiquidEnabled {
		var liquidNetwork *network.Network
		if cfg.LiquidNetworkString == "regtest" {
			liquidNetwork = &network.Regtest
		} else if cfg.LiquidNetworkString == "testnet" {
			liquidNetwork = &network.Testnet
		} else {
			liquidNetwork = &network.Liquid
		}
		cfg.LiquidNetwork = liquidNetwork

		if cfg.LiquidRpcPasswordFile != "" {
			passBytes, err := ioutil.ReadFile(cfg.LiquidRpcPasswordFile)
			if err != nil {
				log.Printf("error reading file: %v", err)
				return err
			}
			passString := strings.TrimRight(string(passBytes), "\r\n")
			cfg.LiquidRpcPassword = passString
		}

	}

	return nil
}

func getBitcoinChain(li *glightning.Lightning) (*chaincfg.Params, error) {
	gi, err := li.GetInfo()
	if err != nil {
		return nil, err
	}
	switch gi.Network {
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
	case "testnet":
		return &chaincfg.TestNet3Params, nil
	case "signet":
		return &chaincfg.SigNetParams, nil
	case "bitcoin":
		return &chaincfg.MainNetParams, nil
	default:
		return nil, errors.New("unknown bitcoin network")
	}
}
func getBitcoinClient(li *glightning.Lightning) (*gbitcoin.Bitcoin, error) {
	configs, err := li.ListConfigs()
	if err != nil {
		return nil, err
	}
	gi, err := li.GetInfo()
	if err != nil {
		return nil, err
	}
	jsonString, err := json.Marshal(configs)
	if err != nil {
		return nil, err
	}
	var listconfigRes *ListConfigRes
	err = json.Unmarshal(jsonString, &listconfigRes)
	if err != nil {
		return nil, err
	}
	var bcliConfig *ImportantPlugin
	for _, v := range listconfigRes.ImportantPlugins {
		if v.Name == "bcli" {
			bcliConfig = v
		}
	}
	if bcliConfig == nil {
		return nil, errors.New("bcli config not found")
	}
	// todo look for overrides in peerswap config
	var bitcoin *gbitcoin.Bitcoin
	rpcUser, ok := bcliConfig.Options["bitcoin-rpcuser"]
	if !ok {

		log.Printf("looking for bitcoin cookie")
		// look for cookie file
		bitcoinDir, ok := bcliConfig.Options["bitcoin-datadir"].(string)
		if !ok {
			log.Printf("no `bitcoin-datadir` config set")
			return nil, nil
		}

		cookiePath := filepath.Join(bitcoinDir, getNetworkFolder(gi.Network), ".cookie")
		if _, err := os.Stat(cookiePath); os.IsNotExist(err) {
			log.Printf("cannot find bitcoin cookie file at %s", cookiePath)
			return nil, nil
		}
		cookieBytes, err := os.ReadFile(cookiePath)
		if err != nil {
			return nil, err
		}

		cookie := strings.Split(string(cookieBytes), ":")
		// use cookie for auth
		bitcoin = gbitcoin.NewBitcoin(cookie[0], cookie[1])

		// assume localhost and standard network ports
		rpcHost := "http://127.0.0.1"
		rpcPort := getNetworkPort(gi.Network)
		log.Printf("connecting with %s, %s to %s, %v", cookie[0], cookie[1], rpcHost, rpcPort)
		err = bitcoin.StartUp(rpcHost, "", rpcPort)
		if err != nil {
			return nil, err
		}
	} else {

		// assume auth authentication
		rpcPass, ok := bcliConfig.Options["bitcoin-rpcpassword"]
		if !ok {
			log.Printf("`bitcoin-rpcpassword` not set in lightning config")
			return nil, nil
		}
		bitcoin = gbitcoin.NewBitcoin(rpcUser.(string), rpcPass.(string))
		bitcoin.SetTimeout(10)

		rpcPortStr, ok := bcliConfig.Options["bitcoin-rpcport"]
		if !ok {
			log.Printf("`bitcoin-rpcport` not set in lightning config")
			return nil, nil
		}

		rpcPort, err := strconv.Atoi(rpcPortStr.(string))
		if err != nil {
			return nil, err
		}

		rpcConn, ok := bcliConfig.Options["bitcoin-rpcconnect"]
		var rpcConnStr string
		/* We default to localhost */
		if rpcConn == nil {
			rpcConnStr = "localhost"
		} else {
			rpcConnStr = rpcConn.(string)
		}

		err = bitcoin.StartUp("http://"+rpcConnStr, "", uint(rpcPort))
		if err != nil {
			return nil, err
		}
	}

	return bitcoin, nil
}

func getNetworkFolder(network string) string {
	switch network {
	case "regtest":
		return "regtest"
	case "testnet":
		return "testnet3"
	case "signet":
		return "signet"
	default:
		return ""
	}
}

func getNetworkPort(network string) uint {
	switch network {
	case "regtest":
		return 18332
	case "testnet":
		return 18332
	case "signet":
		return 38332
	default:
		return 8332
	}
}

type ListConfigRes struct {
	ImportantPlugins []*ImportantPlugin `json:"important-plugins"`
}

type ImportantPlugin struct {
	Path    string
	Name    string
	Options map[string]interface{}
}
