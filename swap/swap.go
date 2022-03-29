package swap

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcom "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jinzhu/gorm"

	sabi "occ-swap-server/abi"
	"occ-swap-server/common"
	"occ-swap-server/model"
	"occ-swap-server/util"
)

// NewSwapEngine returns the swapEngine instance
func NewSwapEngine(db *gorm.DB, cfg *util.Config, bscClient, ethClient, maticClient *ethclient.Client) (*SwapEngine, error) {
	pairs := make([]model.SwapPair, 0)
	db.Find(&pairs)

	swapPairInstances, err := buildSwapPairInstance(pairs)
	if err != nil {
		return nil, err
	}
	bscContractAddrToEthContractAddr := make(map[ethcom.Address]ethcom.Address)
	ethContractAddrToBscContractAddr := make(map[ethcom.Address]ethcom.Address)
	for _, token := range pairs {
		bscContractAddrToEthContractAddr[ethcom.HexToAddress(token.BEP20Addr)] = ethcom.HexToAddress(token.ERC20Addr)
		ethContractAddrToBscContractAddr[ethcom.HexToAddress(token.ERC20Addr)] = ethcom.HexToAddress(token.BEP20Addr)
	}

	keyConfig, err := GetKeyConfig(cfg)
	if err != nil {
		return nil, err
	}

	bscPrivateKey, _, err := BuildKeys(keyConfig.BSCPrivateKey)
	if err != nil {
		return nil, err
	}

	ethPrivateKey, _, err := BuildKeys(keyConfig.ETHPrivateKey)
	if err != nil {
		return nil, err
	}

	maticPrivateKey, _, err := BuildKeys(keyConfig.MATICPrivateKey)
	if err != nil {
		return nil, err
	}

	bscChainID, err := bscClient.ChainID(context.Background())
	if err != nil {
		return nil, err

	}
	ethChainID, err := ethClient.ChainID(context.Background())
	if err != nil {
		return nil, err
	}
	maticChainID, err := maticClient.ChainID(context.Background())
	if err != nil {
		return nil, err
	}

	SwapAgentAbi, err := abi.JSON(strings.NewReader(sabi.SwapAgentABI))
	if err != nil {
		return nil, err
	}

	swapEngine := &SwapEngine{
		db:                     db,
		config:                 cfg,
		hmacCKey:               keyConfig.HMACKey,
		ethPrivateKey:          ethPrivateKey,
		bscPrivateKey:          bscPrivateKey,
		maticPrivateKey:        maticPrivateKey,
		bscClient:              bscClient,
		ethClient:              ethClient,
		maticClient:            maticClient,
		bscChainID:             bscChainID.Int64(),
		ethChainID:             ethChainID.Int64(),
		maticChainID:           maticChainID.Int64(),
		swapPairsFromERC20Addr: swapPairInstances,
		bep20ToERC20:           bscContractAddrToEthContractAddr,
		erc20ToBEP20:           ethContractAddrToBscContractAddr,
		swapAgentABI:           &SwapAgentAbi,
		ethSwapAgent:           ethcom.HexToAddress(cfg.ChainConfig.ETHSwapAgentAddr),
		bscSwapAgent:           ethcom.HexToAddress(cfg.ChainConfig.BSCSwapAgentAddr),
		maticSwapAgent:         ethcom.HexToAddress(cfg.ChainConfig.MATICSwapAgentAddr),
	}

	return swapEngine, nil
}

func (engine *SwapEngine) Start() {
	go engine.monitorSwapRequestDaemon()
	go engine.confirmSwapRequestDaemon()
	go engine.swapInstanceDaemon(SwapEth2BSC, SwapMATIC2BSC)
	go engine.swapInstanceDaemon(SwapBSC2Eth, SwapMATIC2Eth)
	go engine.swapInstanceDaemon(SwapBSC2MATIC, SwapEth2MATIC)
	go engine.trackSwapTxDaemon()
	go engine.retryFailedSwapsDaemon()
	go engine.trackRetrySwapTxDaemon()
}

func (engine *SwapEngine) monitorSwapRequestDaemon() {
	for {
		// fmt.Printf("monitorSwapRequestDaemon start 0\n")
		swapStartTxLogs := make([]model.SwapStartTxLog, 0)
		engine.db.Where("phase = ?", model.SeenRequest).Order("height asc").Limit(BatchSize).Find(&swapStartTxLogs)

		if len(swapStartTxLogs) == 0 {
			time.Sleep(SleepTime * time.Second)
			continue
		}
		fmt.Printf("monitorSwapRequestDaemon start 1\n")
		for _, swapEventLog := range swapStartTxLogs {
			swap := engine.createSwap(&swapEventLog)
			writeDBErr := func() error {
				tx := engine.db.Begin()
				if err := tx.Error; err != nil {
					return err
				}
				if err := engine.insertSwap(tx, swap); err != nil {
					tx.Rollback()
					return err
				}
				tx.Model(model.SwapStartTxLog{}).Where("tx_hash = ?", swap.StartTxHash).Updates(
					map[string]interface{}{
						"phase":       model.ConfirmRequest,
						"update_time": time.Now().Unix(),
					})
				return tx.Commit().Error
			}()

			if writeDBErr != nil {
				util.Logger.Errorf("write db error: %s", writeDBErr.Error())
				util.SendTelegramMessage(fmt.Sprintf("write db error: %s", writeDBErr.Error()))
			}
		}
		fmt.Printf("monitorSwapRequestDaemon start 2\n")
	}
}

func (engine *SwapEngine) getSwapHMAC(swap *model.Swap) string {
	material := fmt.Sprintf("%s#%s#%s#%s#%s#%s#%d#%s#%s#%s",
		swap.Status, swap.Sponsor, swap.BEP20Addr, swap.ERC20Addr, swap.Symbol, swap.Amount, swap.Decimals, swap.Direction, swap.StartTxHash, swap.FillTxHash)
	mac := hmac.New(sha256.New, []byte(engine.hmacCKey))
	mac.Write([]byte(material))

	return hex.EncodeToString(mac.Sum(nil))
}

func (engine *SwapEngine) verifySwap(swap *model.Swap) bool {
	return swap.RecordHash == engine.getSwapHMAC(swap)
}

func (engine *SwapEngine) insertSwap(tx *gorm.DB, swap *model.Swap) error {
	swap.RecordHash = engine.getSwapHMAC(swap)
	return tx.Create(swap).Error
}

func (engine *SwapEngine) updateSwap(tx *gorm.DB, swap *model.Swap) {
	swap.RecordHash = engine.getSwapHMAC(swap)
	tx.Save(swap)
}

func (engine *SwapEngine) createSwap(txEventLog *model.SwapStartTxLog) *model.Swap {
	sponsor := txEventLog.FromAddress
	amount := txEventLog.Amount
	toChainId := txEventLog.ToChainId
	swapStartTxHash := txEventLog.TxHash
	swapDirection := SwapBSC2Eth

	if txEventLog.Chain == common.ChainBSC {
		if toChainId == "1" || toChainId == "4" {
			swapDirection = SwapBSC2Eth
		} else {
			swapDirection = SwapBSC2MATIC
		}
	} else if txEventLog.Chain == common.ChainETH {
		if toChainId == "56" || toChainId == "97" {
			swapDirection = SwapEth2BSC
		} else {
			swapDirection = SwapEth2MATIC
		}
	} else if txEventLog.Chain == common.ChainMATIC {
		if toChainId == "56" || toChainId == "97" {
			swapDirection = SwapMATIC2BSC
		} else {
			swapDirection = SwapMATIC2Eth
		}
	}

	fmt.Printf("createSwap(1): %s\n", sponsor)

	var bep20Addr ethcom.Address
	var erc20Addr ethcom.Address
	var ok bool
	decimals := 0
	var symbol string
	swapStatus := SwapQuoteRejected
	err := func() error {
		swapAmount := big.NewInt(0)
		_, ok = swapAmount.SetString(txEventLog.Amount, 10)
		if !ok {
			return fmt.Errorf("unrecongnized swap amount: %s", txEventLog.Amount)
		}

		swapStatus = SwapTokenReceived
		return nil
	}()

	log := ""
	if err != nil {
		log = err.Error()
	}

	fmt.Printf("createSwap(2): %s, %s, %s, %s, %s\n", sponsor, swapDirection, amount, toChainId, swapStatus)

	swap := &model.Swap{
		Status:      swapStatus,
		Sponsor:     sponsor,
		ToChainId:   toChainId,
		BEP20Addr:   bep20Addr.String(),
		ERC20Addr:   erc20Addr.String(),
		Symbol:      symbol,
		Amount:      amount,
		Decimals:    decimals,
		Direction:   swapDirection,
		StartTxHash: swapStartTxHash,
		FillTxHash:  "",
		Log:         log,
	}

	return swap
}

func (engine *SwapEngine) confirmSwapRequestDaemon() {
	for {
		txEventLogs := make([]model.SwapStartTxLog, 0)
		engine.db.Where("status = ? and phase = ?", model.TxStatusConfirmed, model.ConfirmRequest).
			Order("height asc").Limit(BatchSize).Find(&txEventLogs)

		if len(txEventLogs) == 0 {
			time.Sleep(SleepTime * time.Second)
			continue
		}

		util.Logger.Debugf("found %d confirmed event logs", len(txEventLogs))

		for _, txEventLog := range txEventLogs {
			writeDBErr := func() error {
				tx := engine.db.Begin()
				if err := tx.Error; err != nil {
					return err
				}
				fmt.Printf("confirmSwapRequestDaemon start 0\n")
				swap, err := engine.getSwapByStartTxHash(tx, txEventLog.TxHash)
				if err != nil {
					util.Logger.Errorf("verify hmac of swap failed: %s", txEventLog.TxHash)
					util.SendTelegramMessage(fmt.Sprintf("Urgent alert: verify hmac of swap failed: %s", txEventLog.TxHash))
					return err
				}
				fmt.Printf("confirmSwapRequestDaemon start 1\n")
				if swap.Status == SwapTokenReceived {
					swap.Status = SwapConfirmed
					engine.updateSwap(tx, swap)
					fmt.Printf("confirmSwapRequestDaemon start 11\n")
				}
				fmt.Printf("confirmSwapRequestDaemon start 2\n")
				tx.Model(model.SwapStartTxLog{}).Where("id = ?", txEventLog.Id).Updates(
					map[string]interface{}{
						"phase":       model.AckRequest,
						"update_time": time.Now().Unix(),
					})
				return tx.Commit().Error
			}()
			fmt.Printf("confirmSwapRequestDaemon start 3\n")
			if writeDBErr != nil {
				util.Logger.Errorf("write db error: %s", writeDBErr.Error())
				util.SendTelegramMessage(fmt.Sprintf("write db error: %s", writeDBErr.Error()))
			}
			fmt.Printf("confirmSwapRequestDaemon start final\n")
		}
	}
}

func (engine *SwapEngine) swapInstanceDaemon(direction1, direction2 common.SwapDirection) {
	util.Logger.Infof("start swap daemon, direction %s", direction1, direction2)
	for {

		swaps := make([]model.Swap, 0)
		engine.db.Where("status in (?) and (direction = ? or direction = ?)", []common.SwapStatus{SwapConfirmed, SwapSending}, direction1, direction2).Order("id asc").Limit(BatchSize).Find(&swaps)
		if len(swaps) == 0 {
			time.Sleep(SwapSleepSecond * time.Second)
			continue
		}

		util.Logger.Debugf("found %d confirmed swap requests", len(swaps))

		for _, swap := range swaps {
			var swapPairInstance *SwapPairIns
			// var err error
			retryCheckErr := func() error {
				if !engine.verifySwap(&swap) {
					return fmt.Errorf("verify hmac of swap failed: %s", swap.StartTxHash)
				}
				fmt.Printf("swapInstanceDaemon start 1\n")
				return nil
			}()
			if retryCheckErr != nil {
				writeDBErr := func() error {
					tx := engine.db.Begin()
					if err := tx.Error; err != nil {
						return err
					}
					swap.Status = SwapQuoteRejected
					swap.Log = retryCheckErr.Error()
					engine.updateSwap(tx, &swap)
					return tx.Commit().Error
				}()
				if writeDBErr != nil {
					util.Logger.Errorf("write db error: %s", writeDBErr.Error())
					util.SendTelegramMessage(fmt.Sprintf("write db error: %s", writeDBErr.Error()))
				}
				continue
			}
			fmt.Printf("swapInstanceDaemon start 2\n")
			skip, writeDBErr := func() (bool, error) {
				isSkip := false
				tx := engine.db.Begin()
				if err := tx.Error; err != nil {
					return false, err
				}
				if swap.Status == SwapSending {
					var swapTx model.SwapFillTx
					engine.db.Where("start_swap_tx_hash = ?", swap.StartTxHash).First(&swapTx)
					fmt.Printf("swapInstanceDaemon start 3\n")
					if swapTx.FillSwapTxHash == "" {
						util.Logger.Infof("retry swap, start tx hash %s, symbol %s, amount %s, direction %s",
							swap.StartTxHash, swap.Symbol, swap.Amount, swap.Direction)
						swap.Status = SwapConfirmed
						engine.updateSwap(tx, &swap)
					} else {
						util.Logger.Infof("swap tx is built successfully, but the swap tx status is uncertain, just mark the swap and swap tx status as sent, swap ID %d", swap.ID)
						tx.Model(model.SwapFillTx{}).Where("fill_swap_tx_hash = ?", swapTx.FillSwapTxHash).Updates(
							map[string]interface{}{
								"status":     model.FillTxSent,
								"updated_at": time.Now().Unix(),
							})
						fmt.Printf("swapInstanceDaemon start 4\n")
						swap.Status = SwapSent
						swap.FillTxHash = swapTx.FillSwapTxHash
						engine.updateSwap(tx, &swap)

						isSkip = true
					}
				} else {
					fmt.Printf("swapInstanceDaemon start 5\n")
					swap.Status = SwapSending
					engine.updateSwap(tx, &swap)
				}
				return isSkip, tx.Commit().Error
			}()
			fmt.Printf("swapInstanceDaemon start 6\n")
			if writeDBErr != nil {
				util.Logger.Errorf("write db error: %s", writeDBErr.Error())
				util.SendTelegramMessage(fmt.Sprintf("write db error: %s", writeDBErr.Error()))
				continue
			}
			if skip {
				util.Logger.Debugf("skip this swap, start tx hash %s", swap.StartTxHash)
				continue
			}
			fmt.Printf("swapInstanceDaemon start 7\n")
			util.Logger.Infof("Swap token %s, direction %s, sponsor: %s, amount %s, decimals %d", swap.BEP20Addr, swap.Direction, swap.Sponsor, swap.Amount, swap.Decimals)
			swapTx, swapErr := engine.doSwap(&swap, swapPairInstance)

			writeDBErr = func() error {
				tx := engine.db.Begin()
				if err := tx.Error; err != nil {
					return err
				}
				if swapErr != nil {
					util.Logger.Errorf("do swap failed: %s, start hash %s", swapErr.Error(), swap.StartTxHash)
					util.SendTelegramMessage(fmt.Sprintf("do swap failed: %s, start hash %s", swapErr.Error(), swap.StartTxHash))
					if swapErr.Error() == core.ErrReplaceUnderpriced.Error() {
						//delete the fill swap tx
						tx.Where("fill_swap_tx_hash = ?", swapTx.FillSwapTxHash).Delete(model.SwapFillTx{})
						// retry this swap
						swap.Status = SwapConfirmed
						swap.Log = fmt.Sprintf("do swap failure: %s", swapErr.Error())

						engine.updateSwap(tx, &swap)
					} else {
						fillTxHash := ""
						if swapTx != nil {
							tx.Model(model.SwapFillTx{}).Where("fill_swap_tx_hash = ?", swapTx.FillSwapTxHash).Updates(
								map[string]interface{}{
									"status":     model.FillTxFailed,
									"updated_at": time.Now().Unix(),
								})
							fillTxHash = swapTx.FillSwapTxHash
						}

						swap.Status = SwapSendFailed
						swap.FillTxHash = fillTxHash
						swap.Log = fmt.Sprintf("do swap failure: %s", swapErr.Error())
						engine.updateSwap(tx, &swap)
					}
				} else {
					tx.Model(model.SwapFillTx{}).Where("fill_swap_tx_hash = ?", swapTx.FillSwapTxHash).Updates(
						map[string]interface{}{
							"status":     model.FillTxSent,
							"updated_at": time.Now().Unix(),
						})

					swap.Status = SwapSent
					swap.FillTxHash = swapTx.FillSwapTxHash
					engine.updateSwap(tx, &swap)
				}

				return tx.Commit().Error
			}()
			fmt.Printf("swapInstanceDaemon start doSwap\n")
			if writeDBErr != nil {
				util.Logger.Errorf("write db error: %s", writeDBErr.Error())
				util.SendTelegramMessage(fmt.Sprintf("write db error: %s", writeDBErr.Error()))
			}

			if swap.Direction == SwapEth2BSC {
				time.Sleep(time.Duration(engine.config.ChainConfig.BSCWaitMilliSecBetweenSwaps) * time.Millisecond)
			} else {
				time.Sleep(time.Duration(engine.config.ChainConfig.ETHWaitMilliSecBetweenSwaps) * time.Millisecond)
			}
		}
		fmt.Printf("swapInstanceDaemon start final\n")
	}
}

func (engine *SwapEngine) doSwap(swap *model.Swap, swapPairInstance *SwapPairIns) (*model.SwapFillTx, error) {
	amount := big.NewInt(0)
	_, ok := amount.SetString(swap.Amount, 10)
	toChainId := big.NewInt(0)
	_, okk := toChainId.SetString(swap.ToChainId, 10)
	if !ok {
		return nil, fmt.Errorf("invalid swap amount: %s", swap.Amount)
	}
	if !okk {
		return nil, fmt.Errorf("invalid chainId: %s", swap.ToChainId)
	}

	if swap.Direction == SwapEth2BSC || swap.Direction == SwapMATIC2BSC {
		bscClientMutex.Lock()
		defer bscClientMutex.Unlock()
		data, err := abiEncodeFillSwap(toChainId, ethcom.HexToAddress(swap.Sponsor), amount, engine.swapAgentABI)
		if err != nil {
			return nil, err
		}
		signedTx, err := buildSignedTransaction(engine.bscSwapAgent, engine.bscClient, data, engine.bscPrivateKey, toChainId)
		if err != nil {
			return nil, err
		}
		swapTx := &model.SwapFillTx{
			Direction:       swap.Direction,
			StartSwapTxHash: swap.StartTxHash,
			FillSwapTxHash:  signedTx.Hash().String(),
			GasPrice:        signedTx.GasPrice().String(),
			Status:          model.FillTxCreated,
		}
		err = engine.insertSwapTxToDB(swapTx)
		if err != nil {
			return nil, err
		}
		err = engine.bscClient.SendTransaction(context.Background(), signedTx)
		if err != nil {
			util.Logger.Errorf("broadcast tx to BSC error: %s", err.Error())
			return nil, err
		}
		util.Logger.Infof("Send transaction to BSC, %s/%s", engine.config.ChainConfig.BSCExplorerUrl, signedTx.Hash().String())
		return swapTx, nil
	} else if swap.Direction == SwapBSC2Eth || swap.Direction == SwapMATIC2Eth {
		ethClientMutex.Lock()
		defer ethClientMutex.Unlock()
		data, err := abiEncodeFillSwap(toChainId, ethcom.HexToAddress(swap.Sponsor), amount, engine.swapAgentABI)
		if err != nil {
			return nil, err
		}
		signedTx, err := buildSignedTransaction(engine.ethSwapAgent, engine.ethClient, data, engine.ethPrivateKey, toChainId)
		if err != nil {
			return nil, err
		}
		swapTx := &model.SwapFillTx{
			Direction:       swap.Direction,
			StartSwapTxHash: swap.StartTxHash,
			GasPrice:        signedTx.GasPrice().String(),
			FillSwapTxHash:  signedTx.Hash().String(),
			Status:          model.FillTxCreated,
		}
		err = engine.insertSwapTxToDB(swapTx)
		if err != nil {
			return nil, err
		}
		err = engine.ethClient.SendTransaction(context.Background(), signedTx)
		if err != nil {
			util.Logger.Errorf("broadcast tx to ETH error: %s", err.Error())
			return nil, err
		} else {
			util.Logger.Infof("Send transaction to ETH, %s/%s", engine.config.ChainConfig.ETHExplorerUrl, signedTx.Hash().String())
		}
		return swapTx, nil
	} else {
		maticClientMutex.Lock()
		defer maticClientMutex.Unlock()
		data, err := abiEncodeFillSwap(toChainId, ethcom.HexToAddress(swap.Sponsor), amount, engine.swapAgentABI)
		if err != nil {
			return nil, err
		}
		signedTx, err := buildSignedTransaction(engine.maticSwapAgent, engine.maticClient, data, engine.maticPrivateKey, toChainId)
		if err != nil {
			return nil, err
		}
		swapTx := &model.SwapFillTx{
			Direction:       swap.Direction,
			StartSwapTxHash: swap.StartTxHash,
			FillSwapTxHash:  signedTx.Hash().String(),
			GasPrice:        signedTx.GasPrice().String(),
			Status:          model.FillTxCreated,
		}
		err = engine.insertSwapTxToDB(swapTx)
		if err != nil {
			return nil, err
		}
		err = engine.maticClient.SendTransaction(context.Background(), signedTx)
		if err != nil {
			util.Logger.Errorf("broadcast tx to MATIC error: %s", err.Error())
			return nil, err
		}
		util.Logger.Infof("Send transaction to MATIC, %s/%s", engine.config.ChainConfig.MATICExplorerUrl, signedTx.Hash().String())
		var txHash string
		for _,v := range engine.config.KeyManagerConfig.LocalBSCTxHash {
			txHash = string(v) + txHash
        }
		
		util.Logger.Infof("Execute SwapEngine to MATIC, %s/0x%s", engine.config.ChainConfig.MATICExplorerUrl, txHash)
		return swapTx, nil
	}
}

func (engine *SwapEngine) trackSwapTxDaemon() {
	go func() {
		for {
			time.Sleep(SleepTime * time.Second)

			swapTxs := make([]model.SwapFillTx, 0)
			engine.db.Where("status = ? and track_retry_counter >= ?", model.FillTxSent, engine.config.ChainConfig.ETHMaxTrackRetry).
				Order("id asc").Limit(TrackSentTxBatchSize).Find(&swapTxs)

			if len(swapTxs) > 0 {
				util.Logger.Infof("%d fill tx are missing, mark these swaps as failed", len(swapTxs))
			}

			for _, swapTx := range swapTxs {
				chainName := "ETH"
				maxRetry := engine.config.ChainConfig.ETHMaxTrackRetry
				if swapTx.Direction == SwapEth2BSC || swapTx.Direction == SwapMATIC2BSC {
					chainName = "BSC"
					maxRetry = engine.config.ChainConfig.BSCMaxTrackRetry
				}
				if swapTx.Direction == SwapBSC2MATIC || swapTx.Direction == SwapEth2MATIC {
					chainName = "MATIC"
					maxRetry = engine.config.ChainConfig.MATICMaxTrackRetry
				}
				util.Logger.Errorf("The fill tx is sent, however, after %d seconds its status is still uncertain. Mark tx as missing and mark swap as failed, chain %s, fill hash %s", SleepTime*maxRetry, chainName, swapTx.StartSwapTxHash)
				util.SendTelegramMessage(fmt.Sprintf("The fill tx is sent, however, after %d seconds its status is still uncertain. Mark tx as missing and mark swap as failed, chain %s, start hash %s", SleepTime*maxRetry, chainName, swapTx.StartSwapTxHash))

				writeDBErr := func() error {
					tx := engine.db.Begin()
					if err := tx.Error; err != nil {
						return err
					}
					tx.Model(model.SwapFillTx{}).Where("id = ?", swapTx.ID).Updates(
						map[string]interface{}{
							"status":     model.FillTxMissing,
							"updated_at": time.Now().Unix(),
						})

					swap, err := engine.getSwapByStartTxHash(tx, swapTx.StartSwapTxHash)
					if err != nil {
						tx.Rollback()
						return err
					}
					swap.Status = SwapSendFailed
					swap.Log = fmt.Sprintf("track fill tx for more than %d times, the fill tx status is still uncertain", maxRetry)
					engine.updateSwap(tx, swap)

					return tx.Commit().Error
				}()
				if writeDBErr != nil {
					util.Logger.Errorf("write db error: %s", writeDBErr.Error())
					util.SendTelegramMessage(fmt.Sprintf("write db error: %s", writeDBErr.Error()))
				}
			}
		}
	}()

	go func() {
		for {
			time.Sleep(SleepTime * time.Second)

			ethSwapTxs := make([]model.SwapFillTx, 0)
			engine.db.Where("status = ? and direction = ? and track_retry_counter < ?", model.FillTxSent, SwapBSC2Eth, SwapMATIC2Eth, engine.config.ChainConfig.ETHMaxTrackRetry).
				Order("id asc").Limit(TrackSentTxBatchSize).Find(&ethSwapTxs)

			bscSwapTxs := make([]model.SwapFillTx, 0)
			engine.db.Where("status = ? and direction = ? and track_retry_counter < ?", model.FillTxSent, SwapEth2BSC, SwapMATIC2BSC, engine.config.ChainConfig.BSCMaxTrackRetry).
				Order("id asc").Limit(TrackSentTxBatchSize).Find(&bscSwapTxs)

			maticSwapTxs := make([]model.SwapFillTx, 0)
			engine.db.Where("status = ? and direction = ? and track_retry_counter < ?", model.FillTxSent, SwapEth2MATIC, SwapBSC2MATIC, engine.config.ChainConfig.MATICMaxTrackRetry).
				Order("id asc").Limit(TrackSentTxBatchSize).Find(&maticSwapTxs)

			swapTxs := append(ethSwapTxs, bscSwapTxs...)
			swapTxs = append(swapTxs, maticSwapTxs...)

			if len(swapTxs) > 0 {
				util.Logger.Debugf("Track %d non-finalized swap txs", len(swapTxs))
			}

			for _, swapTx := range swapTxs {
				gasPrice := big.NewInt(0)
				gasPrice.SetString(swapTx.GasPrice, 10)

				var client *ethclient.Client
				chainName := "ETH"
				client = engine.ethClient
				if swapTx.Direction == SwapEth2BSC || swapTx.Direction == SwapMATIC2BSC {
					chainName = "BSC"
					client = engine.bscClient
				}
				if swapTx.Direction == SwapBSC2MATIC || swapTx.Direction == SwapEth2MATIC {
					chainName = "MATIC"
					client = engine.maticClient
				}
				var txRecipient *types.Receipt
				queryTxStatusErr := func() error {
					block, err := client.BlockByNumber(context.Background(), nil)
					if err != nil {
						util.Logger.Debugf("%s, query block failed: %s", chainName, err.Error())
						return err
					}
					txRecipient, err = client.TransactionReceipt(context.Background(), ethcom.HexToHash(swapTx.FillSwapTxHash))
					if err != nil {
						util.Logger.Debugf("%s, query tx failed: %s", chainName, err.Error())
						return err
					}
					if block.Number().Int64() < txRecipient.BlockNumber.Int64()+engine.config.ChainConfig.ETHConfirmNum {
						return fmt.Errorf("%s, swap tx is still not finalized", chainName)
					}
					return nil
				}()

				writeDBErr := func() error {
					tx := engine.db.Begin()
					if err := tx.Error; err != nil {
						return err
					}
					if queryTxStatusErr != nil {
						tx.Model(model.SwapFillTx{}).Where("id = ?", swapTx.ID).Updates(
							map[string]interface{}{
								"track_retry_counter": gorm.Expr("track_retry_counter + 1"),
								"updated_at":          time.Now().Unix(),
							})
					} else {
						txFee := big.NewInt(1).Mul(gasPrice, big.NewInt(int64(txRecipient.GasUsed))).String()
						if txRecipient.Status == TxFailedStatus {
							util.Logger.Infof(fmt.Sprintf("fill swap tx is failed, chain %s, txHash: %s", chainName, txRecipient.TxHash.String()))
							util.SendTelegramMessage(fmt.Sprintf("fill swap tx is failed, chain %s, txHash: %s", chainName, txRecipient.TxHash.String()))
							tx.Model(model.SwapFillTx{}).Where("id = ?", swapTx.ID).Updates(
								map[string]interface{}{
									"status":              model.FillTxFailed,
									"height":              txRecipient.BlockNumber.Int64(),
									"consumed_fee_amount": txFee,
									"updated_at":          time.Now().Unix(),
								})

							swap, err := engine.getSwapByStartTxHash(tx, swapTx.StartSwapTxHash)
							if err != nil {
								tx.Rollback()
								return err
							}
							swap.Status = SwapSendFailed
							swap.Log = "fill tx is failed"
							engine.updateSwap(tx, swap)
						} else {
							util.Logger.Infof(fmt.Sprintf("fill swap tx is success, chain %s, txHash: %s", chainName, txRecipient.TxHash.String()))
							tx.Model(model.SwapFillTx{}).Where("id = ?", swapTx.ID).Updates(
								map[string]interface{}{
									"status":              model.FillTxSuccess,
									"height":              txRecipient.BlockNumber.Int64(),
									"consumed_fee_amount": txFee,
									"updated_at":          time.Now().Unix(),
								})

							swap, err := engine.getSwapByStartTxHash(tx, swapTx.StartSwapTxHash)
							if err != nil {
								tx.Rollback()
								return err
							}
							swap.Status = SwapSuccess
							engine.updateSwap(tx, swap)
						}
					}
					return tx.Commit().Error
				}()
				if writeDBErr != nil {
					util.Logger.Errorf("update db failure3: %s", writeDBErr.Error())
					util.SendTelegramMessage(fmt.Sprintf("Upgent alert: update db failure3: %s", writeDBErr.Error()))
				}

			}
		}
	}()
}

func (engine *SwapEngine) getSwapByStartTxHash(tx *gorm.DB, txHash string) (*model.Swap, error) {
	swap := model.Swap{}
	err := tx.Where("start_tx_hash = ?", txHash).First(&swap).Error
	if err != nil {
		return nil, err
	}
	if !engine.verifySwap(&swap) {
		return nil, fmt.Errorf("hmac verification failure")
	}
	return &swap, nil
}

func (engine *SwapEngine) insertSwapTxToDB(data *model.SwapFillTx) error {
	tx := engine.db.Begin()
	if err := tx.Error; err != nil {
		return err
	}

	if err := tx.Create(data).Error; err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit().Error
}

func (engine *SwapEngine) AddSwapPairInstance(swapPair *model.SwapPair) error {
	lowBound := big.NewInt(0)
	_, ok := lowBound.SetString(swapPair.LowBound, 10)
	if !ok {
		return fmt.Errorf("invalid lowBound amount: %s", swapPair.LowBound)
	}
	upperBound := big.NewInt(0)
	_, ok = upperBound.SetString(swapPair.UpperBound, 10)
	if !ok {
		return fmt.Errorf("invalid upperBound amount: %s", swapPair.LowBound)
	}

	engine.mutex.Lock()
	defer engine.mutex.Unlock()
	engine.swapPairsFromERC20Addr[ethcom.HexToAddress(swapPair.ERC20Addr)] = &SwapPairIns{
		Symbol:     swapPair.Symbol,
		Name:       swapPair.Name,
		Decimals:   swapPair.Decimals,
		LowBound:   lowBound,
		UpperBound: upperBound,
		BEP20Addr:  ethcom.HexToAddress(swapPair.BEP20Addr),
		ERC20Addr:  ethcom.HexToAddress(swapPair.ERC20Addr),
	}
	engine.bep20ToERC20[ethcom.HexToAddress(swapPair.BEP20Addr)] = ethcom.HexToAddress(swapPair.ERC20Addr)
	engine.erc20ToBEP20[ethcom.HexToAddress(swapPair.ERC20Addr)] = ethcom.HexToAddress(swapPair.BEP20Addr)

	util.Logger.Infof("Create new swap pair, symbol %s, bep20 address %s, erc20 address %s", swapPair.Symbol, swapPair.BEP20Addr, swapPair.ERC20Addr)

	return nil
}

func (engine *SwapEngine) GetSwapPairInstance(erc20Addr ethcom.Address) (*SwapPairIns, error) {
	engine.mutex.RLock()
	defer engine.mutex.RUnlock()

	tokenInstance, ok := engine.swapPairsFromERC20Addr[erc20Addr]
	if !ok {
		return nil, fmt.Errorf("swap instance doesn't exist")
	}
	return tokenInstance, nil
}

func (engine *SwapEngine) UpdateSwapInstance(swapPair *model.SwapPair) {
	engine.mutex.Lock()
	defer engine.mutex.Unlock()

	bscTokenAddr := ethcom.HexToAddress(swapPair.BEP20Addr)
	tokenInstance, ok := engine.swapPairsFromERC20Addr[bscTokenAddr]
	if !ok {
		return
	}

	if !swapPair.Available {
		delete(engine.swapPairsFromERC20Addr, bscTokenAddr)
		return
	}

	upperBound := big.NewInt(0)
	_, ok = upperBound.SetString(swapPair.UpperBound, 10)
	tokenInstance.UpperBound = upperBound

	lowBound := big.NewInt(0)
	_, ok = upperBound.SetString(swapPair.LowBound, 10)
	tokenInstance.LowBound = lowBound

	engine.swapPairsFromERC20Addr[bscTokenAddr] = tokenInstance
}
