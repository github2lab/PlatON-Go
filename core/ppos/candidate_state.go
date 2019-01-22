package pposm

import (
	"encoding/json"
	"errors"
	"github.com/PlatONnetwork/PlatON-Go/common"
	"github.com/PlatONnetwork/PlatON-Go/core/state"
	"github.com/PlatONnetwork/PlatON-Go/core/types"
	"github.com/PlatONnetwork/PlatON-Go/core/vm"
	"github.com/PlatONnetwork/PlatON-Go/log"
	"github.com/PlatONnetwork/PlatON-Go/p2p/discover"
	"github.com/PlatONnetwork/PlatON-Go/params"
	"github.com/PlatONnetwork/PlatON-Go/rlp"
	"math/big"
	"net"
	"strconv"
	"sync"
	"fmt"
	"strings"
	"sort"
)

var (
	CandidateEncodeErr          = errors.New("Candidate encoding err")
	CandidateDecodeErr          = errors.New("Candidate decoding err")
	CandidateEmptyErr           = errors.New("Candidate is empty")
	ContractBalanceNotEnoughErr = errors.New("Contract's balance is not enough")
	CandidateOwnerErr           = errors.New("CandidateOwner Addr is illegal")
	DepositLowErr               = errors.New("Candidate deposit too low")
	WithdrawPriceErr            = errors.New("Withdraw Price err")
	WithdrawLowErr              = errors.New("Withdraw Price too low")
	ParamsIllegalErr			= errors.New("Params illegal")
)

type candidateStorage map[discover.NodeID]*types.Candidate
type refundStorage map[discover.NodeID]types.CandidateQueue

type CandidatePool struct {
	// min deposit allow threshold
	threshold *big.Int
	// min deposit limit percentage
	depositLimit uint64
	// allow put into immedidate condition
	allowed uint64
	// allow immediate elected max count
	maxCount uint64
	// allow witness max count
	maxChair uint64
	// allow block interval for refunds
	RefundBlockNumber uint64

	// previous witness
	preOriginCandidates candidateStorage
	// current witnesses
	originCandidates candidateStorage
	// next witnesses
	nextOriginCandidates candidateStorage
	// immediates
	immediateCandidates candidateStorage
	// reserves
	reserveCandidates candidateStorage
	// refunds
	defeatCandidates refundStorage

	// cache
	immediateCacheArr types.CandidateQueue
	reserveCacheArr   types.CandidateQueue
	//lock              *sync.RWMutex
	lock 				*sync.Mutex
}

//var candidatePool *CandidatePool

// Initialize the global candidate pool object
func NewCandidatePool(configs *params.PposConfig) *CandidatePool {
	//if nil != candidatePool {
	//	return candidatePool
	//}

	log.Debug("Build a New CandidatePool Info ...")
	if "" == strings.TrimSpace(configs.Candidate.Threshold) {
		configs.Candidate.Threshold = "1000000000000000000000000"
	}
	var threshold *big.Int
	if thd, ok := new(big.Int).SetString(configs.Candidate.Threshold, 10); !ok {
		threshold, _ = new(big.Int).SetString("1000000000000000000000000", 10)
	}else {
		threshold = thd
	}
	return &CandidatePool{
		threshold: 			  threshold,
		depositLimit:         configs.Candidate.DepositLimit,
		allowed:              configs.Candidate.Allowed,
		maxCount:             configs.Candidate.MaxCount,
		maxChair:             configs.Candidate.MaxChair,
		RefundBlockNumber:    configs.Candidate.RefundBlockNumber,
		preOriginCandidates:  make(candidateStorage, 0),
		originCandidates:     make(candidateStorage, 0),
		nextOriginCandidates: make(candidateStorage, 0),
		immediateCandidates:  make(candidateStorage, 0),
		reserveCandidates:    make(candidateStorage, 0),
		defeatCandidates:     make(refundStorage, 0),
		immediateCacheArr:    make(types.CandidateQueue, 0),
		reserveCacheArr:      make(types.CandidateQueue, 0),
		//lock:                 &sync.RWMutex{},
		lock:                 &sync.Mutex{},
	}
	//return candidatePool
}

// flag:
// 0: only init previous witness and current witness and next witness
// 1：init previous witness and current witness and next witness and immediate and reserve
// 2: init all information
func (c *CandidatePool) initDataByState(state vm.StateDB, flag int) error {
	log.Info("init data by stateDB...", "flag", flag)
	//loading  candidates func
	loadWitFunc := func(title string, canMap candidateStorage,
		getIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
		getInfoFn func(state vm.StateDB, id discover.NodeID) (*types.Candidate, error)) error {
		var witnessIds []discover.NodeID
		if ids, err := getIndexFn(state); nil != err {
			log.Error("Failed to decode "+title+" witnessIds on initDataByState", " err", err)
			return err
		} else {
			witnessIds = ids
		}

		PrintObject(title+" witnessIds", witnessIds)
		for _, witnessId := range witnessIds {

			if ca, err := getInfoFn(state, witnessId); nil != err {
				log.Error("Failed to decode "+title+" witness Candidate on initDataByState", "err", err)
				return CandidateDecodeErr
			} else {
				if nil != ca {
					PrintObject(title+" Id:"+witnessId.String()+", can", ca)
					canMap[witnessId] = ca
				} else {
					delete(canMap, witnessId)
				}
			}
		}
		return nil
	}

	witErrCh := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(3)

	// loading witnesses
	go func() {
		c.preOriginCandidates = make(candidateStorage, 0)
		witErrCh <- loadWitFunc("previous", c.preOriginCandidates, getPreviousWitnessIdsState, getPreviousWitnessByState)
		wg.Done()
	}()
	go func() {
		c.originCandidates = make(candidateStorage, 0)
		witErrCh <- loadWitFunc("current", c.originCandidates, getWitnessIdsByState, getWitnessByState)
		wg.Done()
	}()
	go func() {
		c.nextOriginCandidates = make(candidateStorage, 0)
		witErrCh <- loadWitFunc("next", c.nextOriginCandidates, getNextWitnessIdsByState, getNextWitnessByState)
		wg.Done()
	}()
	var err error
	for i := 1; i <= 3; i++ {
		if err = <-witErrCh; nil != err {
			break
		}
	}
	wg.Wait()
	close(witErrCh)
	if nil != err {
		return err
	}

	// loading elected candidates
	if flag == 1 || flag == 2 {

		loadElectedFunc := func(title string, canMap candidateStorage,
			getIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
			getInfoFn func(state vm.StateDB, id discover.NodeID) (*types.Candidate, error)) (types.CandidateQueue, error) {
			var witnessIds []discover.NodeID

			if ids, err := getIndexFn(state); nil != err {
				log.Error("Failed to decode "+title+"Ids on initDataByState", " err", err)
				return nil, err
			} else {
				witnessIds = ids
			}
			// cache
			canCache := make(types.CandidateQueue, 0)

			PrintObject(title+" Ids", witnessIds)
			for _, witnessId := range witnessIds {

				if ca, err := getInfoFn(state, witnessId); nil != err {
					log.Error("Failed to decode "+title+" Candidate on initDataByState", "err", err)
					return nil, CandidateDecodeErr
				} else {
					if nil != ca {
						PrintObject(title+" Id:"+witnessId.String()+", can", ca)
						canMap[witnessId] = ca
						canCache = append(canCache, ca)
					} else {
						delete(canMap, witnessId)
					}
				}
			}
			return canCache, nil
		}
		type result struct {
			Type int // 1: immediate; 2: reserve
			Arr  types.CandidateQueue
			Err  error
		}
		resCh := make(chan *result, 2)
		wg.Add(2)
		go func() {
			res := new(result)
			res.Type = IS_IMMEDIATE
			c.immediateCandidates = make(candidateStorage, 0)
			if arr, err := loadElectedFunc("immediate", c.immediateCandidates, getImmediateIdsByState, getImmediateByState); nil != err {
				res.Err = err
				resCh <- res
			} else {
				res.Arr = arr
				resCh <- res
			}
			wg.Done()
		}()
		go func() {
			res := new(result)
			res.Type = IS_RESERVE
			c.reserveCandidates = make(candidateStorage, 0)
			if arr, err := loadElectedFunc("reserve", c.reserveCandidates, getReserveIdsByState, getReserveByState); nil != err {
				res.Err = err
				resCh <- res
			} else {
				res.Arr = arr
				resCh <- res
			}
			wg.Done()
		}()
		wg.Wait()
		close(resCh)
		for res := range resCh {
			if nil != res.Err {
				return res.Err
			}
			switch res.Type {
			case IS_IMMEDIATE:
				c.immediateCacheArr = res.Arr
			case IS_RESERVE:
				c.reserveCacheArr = res.Arr
			default:
				continue
			}
		}

	}

	// load refunds
	if flag == 2 {

		var defeatIds []discover.NodeID
		c.defeatCandidates = make(refundStorage, 0)
		if ids, err := getDefeatIdsByState(state); nil != err {
			log.Error("Failed to decode defeatIds on initDataByState", "err", err)
			return err
		} else {
			defeatIds = ids
		}
		PrintObject("defeatIds", defeatIds)
		for _, defeatId := range defeatIds {
			if arr, err := getDefeatsByState(state, defeatId); nil != err {
				log.Error("Failed to decode defeat CandidateArr on initDataByState", "err", err)
				return CandidateDecodeErr
			} else {
				if nil != arr && len(arr) != 0 {
					c.defeatCandidates[defeatId] = arr
				} else {
					delete(c.defeatCandidates, defeatId)
				}
			}
		}
	}
	return nil
}

// pledge Candidate
func (c *CandidatePool) SetCandidate(state vm.StateDB, nodeId discover.NodeID, can *types.Candidate) error {
	defer func() {
		c.ForEachStorage(state, "View State After SetCandidate ...")
	}()
	var nodeIds []discover.NodeID
	c.lock.Lock()

	PrintObject("Call SetCandidate start ...", *can)
	if err := c.initDataByState(state, 2); nil != err {
		c.lock.Unlock()
		log.Error("Failed to initDataByState on SetCandidate", "nodeId", nodeId.String(), " err", err)
		return err
	}

	// 如果是首次质押，判断质押门槛
	if !c.checkFirstThreshold(can) {
		c.lock.Unlock()
		log.Warn("Failed to checkFirstThreshold on SetCandidate", "Deposit", can.Deposit.Uint64(), "threshold", c.threshold)
		return errors.New(DepositLowErr.Error() + ", Current Deposit:" + can.Deposit.String() + ", target threshold:" + fmt.Sprint(c.threshold))
	}

	// 每次质押前都需要先校验 当前can的质押金是否 不小于 要放置的对应的队列是否已满时的最小can的质押金
	if _, ok := c.checkDeposit(state, can); !ok {
		c.lock.Unlock()
		log.Warn("Failed to checkDeposit on SetCandidate", "nodeId", nodeId.String(), " err", DepositLowErr)
		return DepositLowErr
	}

	if arr, err := c.setCandidateInfo(state, nodeId, can); nil != err {
		c.lock.Unlock()
		log.Error("Failed to setCandidateInfo on SetCandidate", "nodeId", nodeId.String(), "err", err)
		return err
	} else {
		nodeIds = arr
	}
	c.lock.Unlock()
	//go ticketPool.DropReturnTicket(state, nodeIds...)
	if len(nodeIds) > 0 {
		if err := ticketPool.DropReturnTicket(state, can.BlockNumber, nodeIds...); nil != err {
			log.Error("Failed to DropReturnTicket on SetCandidate ...")
			//return err
		}
	}
	log.Debug("Call SetCandidate successfully...")
	return nil
}

// 还需要补上 如果TCout 小了，要先移到 reserves 中，不然才算落榜
func (c *CandidatePool) setCandidateInfo(state vm.StateDB, nodeId discover.NodeID, can *types.Candidate) ([]discover.NodeID, error) {


	var flag, delimmediate, delreserve bool
	// check ticket count
	if c.checkTicket(state.TCount(nodeId)) { // TODO
		flag = true
		if _, ok := c.reserveCandidates[can.CandidateId]; ok {
			delreserve = true
		}
		c.immediateCandidates[can.CandidateId] = can
	} else {
		if _, ok := c.immediateCandidates[can.CandidateId]; ok {
			delimmediate = true
		}
		c.reserveCandidates[can.CandidateId] = can
	}

	// cache
	cacheArr := make(types.CandidateQueue, 0)
	if flag {
		for _, v := range c.immediateCandidates {
			cacheArr = append(cacheArr, v)
		}
	} else {
		for _, v := range c.reserveCandidates {
			cacheArr = append(cacheArr, v)
		}
	}

	// sort cache array
	//candidateSort(cacheArr)
	//cacheArr.CandidateSort()
	makeCandidateSort(state, cacheArr)

	// nodeIds cache for lost elected
	nodeIds := make([]discover.NodeID, 0)

	if len(cacheArr) > int(c.maxCount) {
		// Intercepting the lost candidates to tmpArr
		tmpArr := (cacheArr)[c.maxCount:]
		// qualified elected candidates
		cacheArr = (cacheArr)[:c.maxCount]

		// handle tmpArr
		for _, tmpCan := range tmpArr {

			if flag {
				// delete the lost candidates from immediate elected candidates of trie
				c.delImmediate(state, tmpCan.CandidateId)
			} else {
				// delete the lost candidates from reserve elected candidates of trie
				c.delReserve(state, tmpCan.CandidateId)
			}
			// append to refunds (defeat) trie
			if err := c.setDefeat(state, tmpCan.CandidateId, tmpCan); nil != err {
				return nil, err
			}
			nodeIds = append(nodeIds, tmpCan.CandidateId)
		}

		// update index of refund (defeat) on trie
		if err := c.setDefeatIndex(state); nil != err {
			return nil, err
		}
	}

	handleFunc := func(setInfoFn func(state vm.StateDB, candidateId discover.NodeID, can *types.Candidate) error,
		setIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error) error {

		// cache id
		sortIds := make([]discover.NodeID, 0)

		// insert elected candidate to tire
		for _, can := range cacheArr {
			if err := setInfoFn(state, can.CandidateId, can); nil != err {
				return err
			}
			sortIds = append(sortIds, can.CandidateId)
		}
		// update index of elected candidates on trie
		if err := setIndexFn(state, sortIds); nil != err {
			return err
		}
		return nil
	}

	delFunc := func(title string, delInfoFn func(state vm.StateDB, candidateId discover.NodeID),
		getIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
		setIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error) error {

		delInfoFn(state, can.CandidateId)
		// update reserve id index
		if ids, err := getIndexFn(state); nil != err {
			log.Error("withdraw failed get"+title+"Index on setCandidateInfo", "err", err)
			return err
		} else {
			//for i, id := range ids {
			for i := 0; i < len(ids); i++ {
				id := ids[i]
				if id == can.CandidateId {
					ids = append(ids[:i], ids[i+1:]...)
					i--
				}
			}
			if err := setIndexFn(state, ids); nil != err {
				log.Error("withdraw failed set"+title+"Index on setCandidateInfo", "err", err)
				return err
			}
		}
		return nil
	}

	// cache id
	//sortIds := make([]discover.NodeID, 0)
	if flag {
		/** first delete this can on reserves */
		if delreserve {
			if err := delFunc("Reserve", c.delReserve, c.getReserveIndex, c.setReserveIndex); nil != err {
				return nil, err
			}
		}
		c.immediateCacheArr = cacheArr
		return nodeIds, handleFunc(c.setImmediate, c.setImmediateIndex)
	} else {
		/** first delete this can on immediates */
		if delimmediate {
			if err := delFunc("Immediate", c.delImmediate, c.getImmediateIndex, c.setImmediateIndex); nil != err {
				return nil, err
			}
		}
		c.reserveCacheArr = cacheArr
		return nodeIds, handleFunc(c.setReserve, c.setReserveIndex)
	}
}

// Getting immediate or reserve candidate info by nodeId
func (c *CandidatePool) GetCandidate(state vm.StateDB, nodeId discover.NodeID) (*types.Candidate, error) {
	return c.getCandidate(state, nodeId)
}

// Getting immediate or reserve candidate info arr by nodeIds
func (c *CandidatePool) GetCandidateArr(state vm.StateDB, nodeIds ...discover.NodeID) (types.CandidateQueue, error) {
	return c.getCandidates(state, nodeIds...)
}

// candidate withdraw from immediates or reserve elected candidates
func (c *CandidatePool) WithdrawCandidate(state vm.StateDB, nodeId discover.NodeID, price, blockNumber *big.Int) error {
	defer func() {
		c.ForEachStorage(state, "View State After WithdrawCandidate ...")
	}()
	var nodeIds []discover.NodeID
	if arr, err := c.withdrawCandidate(state, nodeId, price, blockNumber); nil != err {
		return err
	} else {
		nodeIds = arr
	}
	//go ticketPool.DropReturnTicket(state, nodeIds...)
	if len(nodeIds) > 0 {
		if err := ticketPool.DropReturnTicket(state, blockNumber, nodeIds...); nil != err {
			log.Error("Failed to DropReturnTicket on WithdrawCandidate ...")
		}
	}
	return nil
}

func (c *CandidatePool) withdrawCandidate(state vm.StateDB, nodeId discover.NodeID, price, blockNumber *big.Int) ([]discover.NodeID, error) {
	log.Info("WithdrawCandidate...", "nodeId", nodeId.String(), "price", price.String())
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 2); nil != err {
		log.Error("Failed to initDataByState on WithdrawCandidate", " err", err)
		return nil, err
	}

	if price.Cmp(new(big.Int).SetUint64(0)) <= 0 {
		log.Error("withdraw failed price invalid", " price", price.String())
		return nil, WithdrawPriceErr
	}
	// cache
	var can *types.Candidate
	var flag bool

	if imCan, ok := c.immediateCandidates[nodeId]; !ok || nil == imCan {
		reCan, ok := c.reserveCandidates[nodeId]
		if !ok || nil == reCan {
			log.Error("withdraw failed current Candidate is empty")
			return nil, CandidateEmptyErr
		} else {
			can = reCan
		}
	} else {
		can = imCan
		flag = true
	}

	// check withdraw price
	if can.Deposit.Cmp(price) < 0 {
		log.Error("withdraw failed refund price must less or equal deposit", "key", nodeId.String())
		return nil, WithdrawPriceErr
	} else if can.Deposit.Cmp(price) == 0 { // full withdraw

		handleFunc := func(tiltle string, delInfoFn func(state vm.StateDB, candidateId discover.NodeID),
			getIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
			setIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error) error {

			// delete current candidate from this elected candidates
			delInfoFn(state, nodeId)
			// update this id index
			if ids, err := getIndexFn(state); nil != err {
				log.Error("withdraw failed get"+tiltle+"Index on full withdraw", "err", err)
				return err
			} else {
				for i := 0; i < len(ids); i++ {
					id := ids[i]
					if id == nodeId {
						ids = append(ids[:i], ids[i+1:]...)
						i--
					}
				}
				if err := setIndexFn(state, ids); nil != err {
					log.Error("withdraw failed set"+tiltle+"Index on full withdraw", "err", err)
					return err
				}
			}
			return nil
		}

		if flag {
			if err := handleFunc("Immediate", c.delImmediate, c.getImmediateIndex, c.setImmediateIndex); nil != err {
				return nil, err
			}

		} else {
			if err := handleFunc("Reserve", c.delReserve, c.getReserveIndex, c.setReserveIndex); nil != err {
				return nil, err
			}
		}

		// append to refund (defeat) trie
		if err := c.setDefeat(state, nodeId, can); nil != err {
			log.Error("withdraw failed setDefeat on full withdraw", "err", err)
			return nil, err
		}
		// update index of defeat on trie
		if err := c.setDefeatIndex(state); nil != err {
			log.Error("withdraw failed setDefeatIndex on full withdraw", "err", err)
			return nil, err
		}

		return []discover.NodeID{nodeId}, nil

	} else { // withdraw a few ...
		// Only withdraw part of the refunds, need to reorder the immediate elected candidates
		// The remaining candiate price to update current candidate info

		if err := c.checkWithdraw(can.Deposit, price); nil != err {
			log.Error("withdraw failed price invalid", " price", price.String(), "err", err)
			return nil, err
		}

		canNew := &types.Candidate{
			Deposit:     new(big.Int).Sub(can.Deposit, price),
			BlockNumber: can.BlockNumber,
			TxIndex:     can.TxIndex,
			CandidateId: can.CandidateId,
			Host:        can.Host,
			Port:        can.Port,
			Owner:       can.Owner,
			From:        can.From,
			Extra:       can.Extra,
			Fee:         can.Fee,
		}

		handleFunc := func(title string, candidateMap candidateStorage,
			setInfoFn func(state vm.StateDB, candidateId discover.NodeID, can *types.Candidate) error,
			setIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error) (types.CandidateQueue, error) {

			// update current candidate
			if err := setInfoFn(state, nodeId, canNew); nil != err {
				log.Error("withdraw failed set"+title+" on a few of withdraw", "err", err)
				return nil, err
			}

			// sort current candidates
			candidateArr := make(types.CandidateQueue, 0)
			for _, can := range candidateMap {
				candidateArr = append(candidateArr, can)
			}
			//candidateSort(candidateArr)
			//candidateArr.CandidateSort()
			makeCandidateSort(state, candidateArr)

			ids := make([]discover.NodeID, 0)
			for _, can := range candidateArr {
				ids = append(ids, can.CandidateId)
			}
			// update new index
			if err := setIndexFn(state, ids); nil != err {
				log.Error("withdraw failed set"+title+"Index on a few of withdraw", "err", err)
				return nil, err
			}
			return candidateArr, nil
		}

		if flag {
			if arr, err := handleFunc("Immediate", c.immediateCandidates, c.setImmediate, c.setImmediateIndex); nil != err {
				return nil, err
			} else {
				c.immediateCacheArr = arr
			}
		} else {
			if arr, err := handleFunc("Reserve", c.reserveCandidates, c.setReserve, c.setReserveIndex); nil != err {
				return nil, err
			} else {
				c.reserveCacheArr = arr
			}
		}

		// the withdraw price to build a new refund into defeat on trie
		canDefeat := &types.Candidate{
			Deposit:     price,
			BlockNumber: blockNumber,
			TxIndex:     can.TxIndex,
			CandidateId: can.CandidateId,
			Host:        can.Host,
			Port:        can.Port,
			Owner:       can.Owner,
			From:        can.From,
			Extra:       can.Extra,
			Fee:         can.Fee,
		}
		// the withdraw
		if err := c.setDefeat(state, nodeId, canDefeat); nil != err {
			log.Error("withdraw failed setDefeat on a few of withdraw", "err", err)
			return nil, err
		}
		// update index of defeat on trie
		if err := c.setDefeatIndex(state); nil != err {
			log.Error("withdraw failed setDefeatIndex on a few of withdraw", "err", err)
			return nil, err
		}
	}
	log.Info("Call WithdrawCandidate SUCCESS !!!!!!!!!!!!")
	return nil, nil
}

// Getting elected candidates array
// flag:
// 0:  Getting all elected candidates array
// 1:  Getting all immediate elected candidates array
// 2:  Getting all reserve elected candidates array
func (c *CandidatePool) GetChosens(state vm.StateDB, flag int) types.CandidateQueue {
	log.Debug("Call GetChosens getting immediate candidates ...")
	c.lock.Lock()
	defer c.lock.Unlock()
	arr := make(types.CandidateQueue, 0)
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on GetChosens", "err", err)
		return arr
	}
	if flag == 0 || flag == 1 {
		immediateIds, err := c.getImmediateIndex(state)
		if nil != err {
			log.Error("Failed to getImmediateIndex on GetChosens", "err", err)
			return arr
		}
		for _, id := range immediateIds {
			if v, ok := c.immediateCandidates[id]; ok {
				arr = append(arr, v)
			}
		}
	}
	if flag == 0 || flag == 2 {
		reserveIds, err := c.getReserveIndex(state)
		if nil != err {
			log.Error("Failed to getReserveIndex on GetChosens", "err", err)
			return make(types.CandidateQueue, 0)
		}
		for _, id := range reserveIds {
			if v, ok := c.reserveCandidates[id]; ok {
				arr = append(arr, v)
			}
		}
	}
	PrintObject("GetChosens ==>", arr)
	return arr
}

// Getting all witness array
func (c *CandidatePool) GetChairpersons(state vm.StateDB) types.CandidateQueue {
	log.Debug("Call GetChairpersons getting witnesses ...")
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 0); nil != err {
		log.Error("Failed to initDataByState on GetChairpersons", "err", err)
		return nil
	}
	witnessIds, err := c.getWitnessIndex(state)
	if nil != err {
		log.Error("Failed to getWitnessIndex on GetChairpersonserr", "err", err)
		return nil
	}
	arr := make(types.CandidateQueue, 0)
	for _, id := range witnessIds {
		if v, ok := c.originCandidates[id]; ok {
			arr = append(arr, v)
		}
	}
	return arr
}

// Getting all refund array by nodeId
func (c *CandidatePool) GetDefeat(state vm.StateDB, nodeId discover.NodeID) (types.CandidateQueue, error) {
	log.Debug("Call GetDefeat getting defeat arr: curr nodeId = " + nodeId.String())
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 2); nil != err {
		log.Error("Failed to initDataByState on GetDefeat", "err", err)
		return nil, err
	}

	defeat, ok := c.defeatCandidates[nodeId]
	if !ok {
		log.Error("Candidate is empty")
		return nil, nil
	}
	return defeat, nil
}

// Checked current candidate was defeat by nodeId
func (c *CandidatePool) IsDefeat(state vm.StateDB, nodeId discover.NodeID) (bool, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 2); nil != err {
		log.Error("Failed to initDataByState on IsDefeat", "err", err)
		return false, err
	}
	if _, ok := c.immediateCandidates[nodeId]; ok {
		return false, nil
	}
	if _, ok := c.reserveCandidates[nodeId]; ok {
		return false, nil
	}
	if arr, ok := c.defeatCandidates[nodeId]; ok && len(arr) != 0 {
		return true, nil
	}
	return false, nil
}

func (c *CandidatePool) IsChosens(state vm.StateDB, nodeId discover.NodeID) (bool, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on IsDefeat", "err", err)
		return false, err
	}
	if _, ok := c.immediateCandidates[nodeId]; ok {
		return true, nil
	}
	if _, ok := c.reserveCandidates[nodeId]; ok {
		return true, nil
	}
	return false, nil
}

// Getting owner's address of candidate info by nodeId
func (c *CandidatePool) GetOwner(state vm.StateDB, nodeId discover.NodeID) common.Address {
	log.Debug("Call GetOwner: curr nodeId = " + nodeId.String())
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 2); nil != err {
		log.Error("Failed to initDataByState on GetOwner", "err", err)
		return common.Address{}
	}
	pre_can, pre_ok := c.preOriginCandidates[nodeId]
	or_can, or_ok := c.originCandidates[nodeId]
	ne_can, ne_ok := c.nextOriginCandidates[nodeId]
	im_can, im_ok := c.immediateCandidates[nodeId]
	re_can, re_ok := c.reserveCandidates[nodeId]
	canArr, de_ok := c.defeatCandidates[nodeId]

	if pre_ok {
		return pre_can.Owner
	}
	if or_ok {
		return or_can.Owner
	}
	if ne_ok {
		return ne_can.Owner
	}
	if im_ok {
		return im_can.Owner
	}
	if re_ok {
		return re_can.Owner
	}
	if de_ok {
		if len(canArr) != 0 {
			return canArr[0].Owner
		}
	}
	return common.Address{}
}

// refund once
func (c *CandidatePool) RefundBalance(state vm.StateDB, nodeId discover.NodeID, blockNumber *big.Int) error {
	defer func() {
		c.ForEachStorage(state, "View State After RefundBalance ...")
	}()
	log.Info("Call RefundBalance:  curr nodeId = " + nodeId.String() + ",curr blocknumber:" + blockNumber.String())
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 2); nil != err {
		log.Error("Failed to initDataByState on RefundBalance", "err", err)
		return err
	}

	var canArr types.CandidateQueue
	if defeatArr, ok := c.defeatCandidates[nodeId]; ok {
		canArr = defeatArr
	} else {
		log.Error("Failed to refundbalance candidate is empty")
		return CandidateDecodeErr
	}
	// cache
	// Used for verification purposes, that is, the beneficiary in the pledge refund information of each nodeId should be the same
	var addr common.Address
	// Grand total refund amount for one-time
	amount := big.NewInt(0)
	// Transfer refund information that needs to be deleted
	delCanArr := make(types.CandidateQueue, 0)

	contractBalance := state.GetBalance(common.CandidatePoolAddr)
	//currentNum := new(big.Int).SetUint64(blockNumber)

	// Traverse all refund information belong to this nodeId
	for index := 0; index < len(canArr); index++ {
		can := canArr[index]
		sub := new(big.Int).Sub(blockNumber, can.BlockNumber)
		log.Info("Check defeat detail", "nodeId:", nodeId.String(), "curr blocknumber:", blockNumber.String(), "setcandidate blocknumber:", can.BlockNumber.String(), " diff:", sub.String())
		if sub.Cmp(new(big.Int).SetUint64(c.RefundBlockNumber)) >= 0 { // allow refund
			delCanArr = append(delCanArr, can)
			canArr = append(canArr[:index], canArr[index+1:]...)
			index--
			// add up the refund price
			amount = new(big.Int).Add(amount, can.Deposit)
			//amount += can.Deposit.Uint64()
		} else {
			log.Warn("block height number had mismatch, No refunds allowed", "current block height", blockNumber.String(), "deposit block height", can.BlockNumber.String(), "nodeId", nodeId.String(), "allowed block interval", c.RefundBlockNumber)
			continue
		}

		if addr == common.ZeroAddr {
			addr = can.Owner
		} else {
			if addr != can.Owner {
				log.Info("Failed to refundbalance couse current nodeId had bind different owner address ", "nodeId", nodeId.String(), "addr1", addr.String(), "addr2", can.Owner)
				if len(canArr) != 0 {
					canArr = append(delCanArr, canArr...)
				} else {
					canArr = delCanArr
				}
				c.defeatCandidates[nodeId] = canArr
				log.Error("Failed to refundbalance Different beneficiary addresses under the same node", "nodeId", nodeId.String(), "addr1", addr.String(), "addr2", can.Owner)
				return CandidateOwnerErr
			}
		}

		// check contract account balance
		//if (contractBalance.Cmp(new(big.Int).SetUint64(amount))) < 0 {
		if (contractBalance.Cmp(amount)) < 0 {
			log.Error("Failed to refundbalance constract account insufficient balance ", "nodeId", nodeId.String(), "contract's balance", state.GetBalance(common.CandidatePoolAddr).String(), "amount", amount.String())
			if len(canArr) != 0 {
				canArr = append(delCanArr, canArr...)
			} else {
				canArr = delCanArr
			}
			c.defeatCandidates[nodeId] = canArr
			return ContractBalanceNotEnoughErr
		}
	}

	// update the tire
	if len(canArr) == 0 {
		c.delDefeat(state, nodeId)
		if ids, err := getDefeatIdsByState(state); nil != err {
			for i := 0; i < len(ids); i++ {
				id := ids[i]
				if id == nodeId {
					ids = append(ids[:i], ids[i+1:]...)
					i--
				}
			}
			if len(ids) != 0 {
				if value, err := rlp.EncodeToBytes(&ids); nil != err {
					log.Error("Failed to encode candidate ids on RefundBalance", "err", err)
					return CandidateEncodeErr
				} else {
					setDefeatIdsState(state, value)
				}
			} else {
				//setDefeatIdsState(state, []byte{})
			}

		}
	} else {
		// If have some remaining, update that
		if arrVal, err := rlp.EncodeToBytes(canArr); nil != err {
			log.Error("Failed to encode candidate object on RefundBalance", "key", nodeId.String(), "err", err)
			canArr = append(delCanArr, canArr...)
			c.defeatCandidates[nodeId] = canArr
			return CandidateDecodeErr
		} else {
			// update the refund information
			setDefeatState(state, nodeId, arrVal)
			// remaining set back to defeat map
			c.defeatCandidates[nodeId] = canArr
		}
	}
	log.Info("Call RefundBalance to tansfer value：", "nodeId", nodeId.String(), "contractAddr", common.CandidatePoolAddr.String(),
		"owner's addr", addr.String(), "Return the amount to be transferred:", amount.String())

	// sub contract account balance
	state.SubBalance(common.CandidatePoolAddr, amount)
	// add owner balace
	state.AddBalance(addr, amount)
	log.Debug("Call RefundBalance success ...")
	return nil
}

// set elected candidate extra value
func (c *CandidatePool) SetCandidateExtra(state vm.StateDB, nodeId discover.NodeID, extra string) error {
	defer func() {
		c.ForEachStorage(state, "View State After SetCandidateExtra ...")
	}()
	log.Info("Call SetCandidateExtra:", "nodeId", nodeId.String(), "extra", extra)
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on SetCandidateExtra", "err", err)
		return err
	}
	if can, ok := c.immediateCandidates[nodeId]; ok {
		// update current candidate info and update to tire
		can.Extra = extra
		if err := c.setImmediate(state, nodeId, can); nil != err {
			log.Error("Failed to setImmediate on SetCandidateExtra", "err", err)
			return err
		}
	} else {
		if can, ok := c.reserveCandidates[nodeId]; ok {
			can.Extra = extra
			if err := c.setReserve(state, nodeId, can); nil != err {
				log.Error("Failed to setReserve on SetCandidateExtra", "err", err)
				return err
			}
		} else {
			return CandidateEmptyErr
		}
	}
	log.Debug("Call SetCandidateExtra SUCCESS !!!!!! ")
	return nil
}

// Announce witness
func (c *CandidatePool) Election(state *state.StateDB, parentHash common.Hash, currBlockNumber *big.Int) ([]*discover.Node, error) {
	defer func() {
		c.ForEachStorage(state, "View State After Election ...")
	}()
	var nodes []*discover.Node
	var cans types.CandidateQueue
	if nodeArr, canArr, err := c.election(state, parentHash); nil != err {
		return nil, err
	} else {
		nodes, cans = nodeArr, canArr
	}

	nodeIds := make([]discover.NodeID, 0)
	for _, can := range cans {
		// 释放幸运票 TODO
		if err := ticketPool.ReturnTicket(state, can.CandidateId, can.TicketId, currBlockNumber); nil != err {
			log.Error("Failed to ReturnTicket on Election", "nodeId", can.CandidateId.String(), "ticketId", can.TicketId.String(), "err", err)
			continue
		}

		/**
		获取TCount
		然后需要对入围候选人再次做排序
		*/
		c.lock.Lock()
		if err := c.initDataByState(state, 2); nil != err {
			c.lock.Unlock()
			log.Error("Failed to initDataByState on Election", "nodeId", can.CandidateId.String(), " err", err)
			return nil, err
		}
		/**
		重新质押前的处理
		 */
		if flag, err := c.preElectionReset(state, can); nil != err {
			c.lock.Unlock()
			log.Error("Failed to preElectionReset on Election", "nodeId", can.CandidateId.String(), " err", err)
			return nil, err
		}else if nil == err && !flag{
			nodeIds = append(nodeIds, can.CandidateId)
			// continue handle next one
			c.lock.Unlock()
			continue
		}
		PrintObject("Election 更新质押 Candidate", *can)
		// 因为需要先判断是否之前在 immediates 中，如果是则转移到 reserves 中
		if ids, err := c.setCandidateInfo(state, can.CandidateId, can); nil != err {
			c.lock.Unlock()
			log.Error("Failed to setCandidateInfo on Election", "nodeId", can.CandidateId.String(), "err", err)
			return nil, err
		} else {
			nodeIds = append(nodeIds, ids...)
		}
		c.lock.Unlock()

	}
	// 释放落榜的
	//go ticketPool.DropReturnTicket(state, nodeIds...)
	if len(nodeIds) > 0 {
		if err := ticketPool.DropReturnTicket(state, currBlockNumber, nodeIds...); nil != err {
			log.Error("Failed to DropReturnTicket on Election ...")
		}
	}
	return nodes, nil
}

func (c *CandidatePool) election(state *state.StateDB, parentHash common.Hash) ([]*discover.Node, types.CandidateQueue, error) {
	log.Info("Call Election start ...", "maxChair", c.maxChair, "maxCount", c.maxCount, "RefundBlockNumber", c.RefundBlockNumber)
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on Election", "err", err)
		return nil, nil, err
	}

	// sort immediate candidates
	//candidateSort(c.immediateCacheArr)
	//c.immediateCacheArr.CandidateSort()
	makeCandidateSort(state, c.immediateCacheArr)

	log.Info("揭榜时，排序的候选池数组长度:", "len", len(c.immediateCacheArr))
	PrintObject("揭榜时，排序的候选池数组:", c.immediateCacheArr)
	// cache ids
	immediateIds := make([]discover.NodeID, 0)
	for _, can := range c.immediateCacheArr {
		immediateIds = append(immediateIds, can.CandidateId)
	}
	log.Info("揭榜时，当前入围者ids 长度：", "len", len(immediateIds))
	PrintObject("揭榜时，当前入围者ids：", immediateIds)
	// a certain number of witnesses in front of the cache
	var nextWitIds []discover.NodeID
	// If the number of candidate selected does not exceed the number of witnesses
	if len(immediateIds) <= int(c.maxChair) {
		nextWitIds = make([]discover.NodeID, len(immediateIds))
		copy(nextWitIds, immediateIds)

	} else {
		// If the number of candidate selected exceeds the number of witnesses, the top N is extracted.
		nextWitIds = make([]discover.NodeID, c.maxChair)
		copy(nextWitIds, immediateIds)
	}
	log.Info("揭榜时，选出来的下一轮见证人Ids 个数:", "len", len(nextWitIds))
	PrintObject("揭榜时，选出来的下一轮见证人Ids:", nextWitIds)
	// cache map
	nextWits := make(candidateStorage, 0)

	// copy witnesses information
	copyCandidateMapByIds(nextWits, c.immediateCandidates, nextWitIds)
	log.Info("揭榜时，从入围信息copy过来的见证人个数;", "len", len(nextWits))
	PrintObject("揭榜时，从入围信息copy过来的见证人;", nextWits)
	// clear all old nextwitnesses information （If it is forked, the next round is no empty.）
	for nodeId, _ := range c.nextOriginCandidates {
		c.delNextWitness(state, nodeId)
	}

	arr := make([]*discover.Node, 0)
	caches := make(types.CandidateQueue, 0)
	// set up all new nextwitnesses information
	//for nodeId, can := range nextWits {
	for _, nodeId := range nextWitIds {
		if can, ok := nextWits[nodeId]; ok {

			// 揭榜后回去调 获取幸运票逻辑 TODO
			luckyId, err := ticketPool.SelectionLuckyTicket(state, nodeId, parentHash)
			if nil != err {
				log.Error("Failed to take luckyId on Election", "nodeId", nodeId.String(), "err", err)
				return nil, nil, err
			}
			//luckyId := common.BytesToHash([]byte("1223"))
			// 将幸运票ID 置入 next witness 详情中
			can.TicketId = luckyId
			if err := c.setNextWitness(state, nodeId, can); nil != err {
				log.Error("failed to setNextWitness on election", "err", err)
				return nil, nil, err
			}
			caches = append(caches, can)
			if node, err := buildWitnessNode(can); nil != err {
				log.Error("Failed to build Node on GetWitness", "err", err, "nodeId", can.CandidateId.String())
				continue
			} else {
				arr = append(arr, node)
			}
		}
	}
	// update new nextwitnesses index
	if err := c.setNextWitnessIndex(state, nextWitIds); nil != err {
		log.Error("failed to setNextWitnessIndex on election", "err", err)
		return nil, nil, err
	}
	// replace the next round of witnesses
	//c.nextOriginCandidates = nextWits

	log.Info("揭榜时，下一轮见证人node 个数:", "len", len(arr))
	PrintObject("揭榜时，下一轮见证人node信息:", arr)
	c.fixElection(state, nextWitIds)
	log.Info("Election next witness's node count:", "len", len(arr))
	log.Info("Call Election SUCCESS !!!!!!!")
	return arr, caches, nil
}

func (c *CandidatePool) fixElection (state vm.StateDB, nextWitIds []discover.NodeID) {
	if len(nextWitIds) != 0 && len(c.nextOriginCandidates) != 0 {
		return
	}
	// 否则，表示当前揭榜出来的 next 为nil，则需要判断 当前 轮是否有 witness，
	// 如果有，则使用当前轮的witness作为 next witness，
	// 储备池的出块奖励需要用到 witness can info
	/*if len(c.originCandidates) != 0 {

	}*/
	log.Info("揭榜时，揭出下轮见证人为 nil, 使用 当前轮作为下一轮...")
	// set up new witnesses to next witnesses on trie by current witnesses
	for nodeId, can := range c.originCandidates {
		if err := c.setNextWitness(state, nodeId, can); nil != err {
			log.Error("Failed to setNextWitness on fixElection", "err", err)
			return
		}
	}
	// update next witness index by current witness index
	if ids, err := c.getWitnessIndex(state); nil != err {
		log.Error("Failed to getWitnessIndex on fixElection", "err", err)
		return
	} else {
		// replace witnesses index
		if err := c.setNextWitnessIndex(state, ids); nil != err {
			log.Error("Failed to setNextWitnessIndex on fixElection", "err", err)
			return
		}
	}
}

// switch next witnesses to current witnesses
func (c *CandidatePool) Switch(state *state.StateDB) bool {
	log.Info("Call Switch start ...")
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 0); nil != err {
		log.Error("Failed to initDataByState on Switch", "err", err)
		return false
	}
	// clear all old previous witness on trie
	for nodeId, _ := range c.preOriginCandidates {
		c.delPreviousWitness(state, nodeId)
	}
	// set up new witnesses to previous witnesses on trie by current witnesses
	for nodeId, can := range c.originCandidates {
		if err := c.setPreviousWitness(state, nodeId, can); nil != err {
			log.Error("Failed to setPreviousWitness on Switch", "err", err)
			return false
		}
	}
	// update previous witness index by current witness index
	if ids, err := c.getWitnessIndex(state); nil != err {
		log.Error("Failed to getWitnessIndex on Switch", "err", err)
		return false
	} else {
		// replace witnesses index
		if err := c.setPreviousWitnessindex(state, ids); nil != err {
			log.Error("Failed to setPreviousWitnessindex on Switch", "err", err)
			return false
		}
	}

	// clear all old witnesses on trie
	for nodeId, _ := range c.originCandidates {
		c.delWitness(state, nodeId)
	}
	// set up new witnesses to current witnesses on trie by next witnesses
	for nodeId, can := range c.nextOriginCandidates {
		if err := c.setWitness(state, nodeId, can); nil != err {
			log.Error("Failed to setWitness on Switch", "err", err)
			return false
		}
	}
	// update current witness index by next witness index
	if ids, err := c.getNextWitnessIndex(state); nil != err {
		log.Error("Failed to getNextWitnessIndex on Switch", "err", err)
		return false
	} else {
		// replace witnesses index
		if err := c.setWitnessindex(state, ids); nil != err {
			log.Error("Failed to setWitnessindex on Switch", "err", err)
			return false
		}
	}
	// clear all old nextwitnesses information
	for nodeId, _ := range c.nextOriginCandidates {
		c.delNextWitness(state, nodeId)
	}
	// clear next witness index
	c.setNextWitnessIndex(state, make([]discover.NodeID, 0))
	log.Info("Call Switch SUCCESS !!!!!!!")
	return true
}

// Getting nodes of witnesses
// flag：-1: the previous round of witnesses  0: the current round of witnesses   1: the next round of witnesses
func (c *CandidatePool) GetWitness(state *state.StateDB, flag int) ([]*discover.Node, error) {
	log.Debug("Call GetWitness: flag = " + strconv.Itoa(flag))
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 0); nil != err {
		log.Error("Failed to initDataByState on GetWitness", "err", err)
		return nil, err
	}
	//var ids []discover.NodeID
	var witness candidateStorage
	var indexArr []discover.NodeID
	if flag == PREVIOUS_C {
		witness = c.preOriginCandidates
		if ids, err := c.getPreviousWitnessIndex(state); nil != err {
			log.Error("Failed to getPreviousWitnessIndex on GetWitness", "err", err)
			return nil, err
		} else {
			indexArr = ids
		}
	} else if flag == CURRENT_C {
		witness = c.originCandidates
		if ids, err := c.getWitnessIndex(state); nil != err {
			log.Error("Failed to getWitnessIndex on GetWitness", "err", err)
			return nil, err
		} else {
			indexArr = ids
		}
	} else if flag == NEXT_C {
		witness = c.nextOriginCandidates
		if ids, err := c.getNextWitnessIndex(state); nil != err {
			log.Error("Failed to getNextWitnessIndex on GetWitness", "err", err)
			return nil, err
		} else {
			indexArr = ids
		}
	}

	arr := make([]*discover.Node, 0)
	for _, id := range indexArr {
		if can, ok := witness[id]; ok {
			if node, err := buildWitnessNode(can); nil != err {
				log.Error("Failed to build Node on GetWitness", "err", err, "nodeId", id.String())
				return nil, err
			} else {
				arr = append(arr, node)
			}
		}
	}
	return arr, nil
}

// Getting previous and current and next witnesses
func (c *CandidatePool) GetAllWitness(state *state.StateDB) ([]*discover.Node, []*discover.Node, []*discover.Node, error) {
	log.Debug("Call GetAllWitness ...")
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 0); nil != err {
		log.Error("Failed to initDataByState on GetAllWitness", "err", err)
		return nil, nil, nil, err
	}

	fetchWitnessFunc := func(title string, witnesses candidateStorage,
		getIndexFn func(state vm.StateDB) ([]discover.NodeID, error)) ([]*discover.Node, error) {

		nodes := make([]*discover.Node, 0)

		// caches
		witIndex := make([]discover.NodeID, 0)

		// getting witness index
		if ids, err := getIndexFn(state); nil != err {
			log.Error("Failed to getting "+title+" witness ids on GetAllWitness", "err", err)
			return nodes, err
		} else {
			witIndex = ids
		}

		// getting witness info
		for _, id := range witIndex {
			if can, ok := witnesses[id]; ok {
				if node, err := buildWitnessNode(can); nil != err {
					log.Error("Failed to build "+title+" Node on GetAllWitness", "err", err, "nodeId", can.CandidateId.String())
					//continue
					return nodes, err
				} else {
					nodes = append(nodes, node)
				}
			}
		}
		return nodes, nil
	}
	preArr, curArr, nextArr := make([]*discover.Node, 0), make([]*discover.Node, 0), make([]*discover.Node, 0)

	type result struct {
		Type  int // -1: previous; 0: current; 1: next
		Err   error
		nodes []*discover.Node
	}
	var wg sync.WaitGroup
	wg.Add(3)
	resCh := make(chan *result, 3)

	go func() {
		res := new(result)
		res.Type = PREVIOUS_C
		if nodes, err := fetchWitnessFunc("previous", c.preOriginCandidates, c.getPreviousWitnessIndex); nil != err {
			res.Err = err
		} else {
			res.nodes = nodes
		}
		resCh <- res
		wg.Done()
	}()
	go func() {
		res := new(result)
		res.Type = CURRENT_C
		if nodes, err := fetchWitnessFunc("current", c.originCandidates, c.getWitnessIndex); nil != err {
			res.Err = err
		} else {
			res.nodes = nodes
		}
		resCh <- res
		wg.Done()
	}()
	go func() {
		res := new(result)
		res.Type = NEXT_C
		if nodes, err := fetchWitnessFunc("next", c.nextOriginCandidates, c.getNextWitnessIndex); nil != err {
			res.Err = err
		} else {
			res.nodes = nodes
		}
		resCh <- res
		wg.Done()
	}()
	wg.Wait()
	close(resCh)
	for res := range resCh {
		if nil != res.Err {
			return nil, nil, nil, res.Err
		}
		switch res.Type {
		case PREVIOUS_C:
			preArr = res.nodes
		case CURRENT_C:
			curArr = res.nodes
		case NEXT_C:
			nextArr = res.nodes
		default:
			continue
		}
	}
	return preArr, curArr, nextArr, nil
}

// Getting can by witnesses
// flag:
// -1: 		previous round
// 0:		current round
// 1: 		next round
func (c *CandidatePool) GetWitnessCandidate (state vm.StateDB, nodeId discover.NodeID, flag int) (*types.Candidate, error) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.initDataByState(state, 0); nil != err {
		log.Error("Failed to initDataByState on GetWitnessCandidate", "err", err)
		return nil, err
	}
	switch flag {
	case PREVIOUS_C:
		if can, ok := c.preOriginCandidates[nodeId]; !ok {
			log.Warn("Call GetWitnessCandidate, can no exist in previous witnesses ", "nodeId", nodeId.String())
			return nil, nil
		}else {
			return can, nil
		}
	case CURRENT_C:
		if can, ok := c.originCandidates[nodeId]; !ok {
			log.Warn("Call GetWitnessCandidate, can no exist in current witnesses", "nodeId", nodeId.String())
			return nil, nil
		}else {
			return can, nil
		}
	case NEXT_C:
		if can, ok := c.nextOriginCandidates[nodeId]; !ok {
			log.Warn("Call GetWitnessCandidate, can no exist in previous witnesses", "nodeId", nodeId.String())
			return nil, nil
		}else {
			return can, nil
		}
	default:
		log.Error("Failed to found can on GetWitnessCandidate, flag is invalid", "flag", flag)
		return nil, errors.New(ParamsIllegalErr.Error() + ", flag:" + fmt.Sprint(flag))
	}
}

func (c *CandidatePool) GetRefundInterval() uint64 {
	return c.RefundBlockNumber
}

// 根据nodeId 去重新决定当前候选人的去留
func (c *CandidatePool) UpdateElectedQueue(state vm.StateDB, currBlockNumber *big.Int, nodeIds ...discover.NodeID) error {
	log.Info("Call UpdateElectedQueue start ...")
	var ids []discover.NodeID
	if arr, err := c.updateQueue(state, nodeIds...); nil != err {
		return err
	} else {
		ids = arr
	}
	log.Info("Call UpdateElectedQueue SUCCESS !!!!!!!!! ")
	c.ForEachStorage(state, "View State After UpdateElectedQueue ...")
	//go ticketPool.DropReturnTicket(state, ids...)
	if len(ids) > 0 {
		return ticketPool.DropReturnTicket(state, currBlockNumber, ids...)
	}
	return nil
}

func (c *CandidatePool) updateQueue(state vm.StateDB, nodeIds ...discover.NodeID) ([]discover.NodeID, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	log.Info("开始更新竞选队列...")
	PrintObject("入参的nodeIds:", nodeIds)
	if len(nodeIds) == 0 {
		log.Debug("updateQueue finish !!!!!!!!!!")
		return nil, nil
	}
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on UpdateElectedQueue", "err", err)
		return nil, err
	}

	handleFunc := func(delTitle, setTitle string, nodeId discover.NodeID, oldMap, newMap candidateStorage,
		delOldInfoFn func(state vm.StateDB, candidateId discover.NodeID),
		delNewInfoFn func(state vm.StateDB, candidateId discover.NodeID),
		setNewInfoFn func(state vm.StateDB, candidateId discover.NodeID, can *types.Candidate) error,
		getOldIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
		setOldIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error,
		setNewIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error,
	) ([]discover.NodeID, types.CandidateQueue, error) {

		log.Warn("处理", "old", delTitle, "new", setTitle)
		PrintObject("oldMap", oldMap)
		PrintObject("newMap", newMap)
		can := oldMap[nodeId]
		newMap[nodeId] = can

		// cache
		cacheArr := make(types.CandidateQueue, 0)
		for _, v := range newMap {
			cacheArr = append(cacheArr, v)
		}

		// sort cache array
		//candidateSort(cacheArr)
		//cacheArr.CandidateSort()
		makeCandidateSort(state, cacheArr)

		// nodeIds cache for lost elected
		cacheNodeIds := make([]discover.NodeID, 0)

		if len(cacheArr) > int(c.maxCount) {
			// Intercepting the lost candidates to tmpArr
			tmpArr := (cacheArr)[c.maxCount:]
			// qualified elected candidates
			cacheArr = (cacheArr)[:c.maxCount]

			// handle tmpArr
			for _, tmpCan := range tmpArr {
				// delete the lost candidates from elected candidates of trie
				delNewInfoFn(state, tmpCan.CandidateId)
				// append to refunds (defeat) trie
				if err := c.setDefeat(state, tmpCan.CandidateId, tmpCan); nil != err {
					return nil, nil, err
				}
				cacheNodeIds = append(cacheNodeIds, tmpCan.CandidateId)
			}

			// update index of refund (defeat) on trie
			if err := c.setDefeatIndex(state); nil != err {
				return nil, nil, err
			}
		}

		/** delete ... */

		delOldInfoFn(state, can.CandidateId)
		// update can's ids index
		if ids, err := getOldIndexFn(state); nil != err {
			log.Error("withdraw failed get"+delTitle+"Index on UpdateElectedQueue", "nodeId", can.CandidateId.String(), "err", err)
			return nil, nil, err
		} else {
			//for i, id := range ids {
			for i := 0; i < len(ids); i++ {
				id := ids[i]
				if id == can.CandidateId {
					ids = append(ids[:i], ids[i+1:]...)
					i--
				}
			}
			if err := setOldIndexFn(state, ids); nil != err {
				log.Error("withdraw failed set"+delTitle+"Index on UpdateElectedQueue", "nodeId", can.CandidateId.String(), "err", err)
				return nil, nil, err
			}
		}

		/** setting ... */

		// cache id
		sortIds := make([]discover.NodeID, 0)

		// insert elected candidate to tire
		for _, can := range cacheArr {
			if err := setNewInfoFn(state, can.CandidateId, can); nil != err {
				log.Error("Failed to set"+setTitle+" on UpdateElectedQueue", "nodeId", can.CandidateId.String(), "err", err)
				return nil, nil, err
			}
			sortIds = append(sortIds, can.CandidateId)
		}
		// update index of elected candidates on trie
		if err := setNewIndexFn(state, sortIds); nil != err {
			log.Error("Failed to set"+setTitle+"Index on UpdateElectedQueue", "nodeId", can.CandidateId.String(), "err", err)
			return nil, nil, err
		}
		return cacheNodeIds, cacheArr, nil
	}

	// 直接落榜并产生退款信息
	directdropFunc := func(title string, can *types.Candidate,
		delInfoFn func(state vm.StateDB, candidateId discover.NodeID),
		getIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
		setIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error) error {

		log.Debug("进入直接掉榜逻辑: 删除 "+title+" 中的can信息 on UpdateElectedQueue", "nodeId", can.CandidateId.String())
		delInfoFn(state, can.CandidateId)

		// append to refunds (defeat) trie
		if err := c.setDefeat(state, can.CandidateId, can); nil != err {
			return err
		}

		// update index of refund (defeat) on trie
		if err := c.setDefeatIndex(state); nil != err {
			return err
		}

		// update reserve id index
		if ids, err := getIndexFn(state); nil != err {
			log.Error("withdraw failed get"+title+"Index on UpdateElectedQueue", "err", err)
			return err
		} else {
			//for i, id := range ids {
			for i := 0; i < len(ids); i++ {
				id := ids[i]
				if id == can.CandidateId {
					ids = append(ids[:i], ids[i+1:]...)
					i--
				}
			}
			if err := setIndexFn(state, ids); nil != err {
				log.Error("withdraw failed set"+title+"Index on UpdateElectedQueue", "err", err)
				return err
			}
		}
		return nil
	}


	delNodeIds := make([]discover.NodeID, 0)


	for _, nodeId := range nodeIds {

		switch c.checkExist(nodeId) {
		case IS_IMMEDIATE:
			log.Debug("当前nodeId原来在 Immediate中...")
			can := c.immediateCandidates[nodeId]
			//if !c.checkTicket(state.TCount(nodeId)) { // TODO
			if tcount, noDrop := c.checkDeposit(state, can); !noDrop { // 直接掉榜
				if err := directdropFunc("Immediate", can, c.delImmediate, c.getImmediateIndex, c.setImmediateIndex); nil != err {
					return nil, err
				}else {
					delNodeIds = append(delNodeIds, nodeId)
				}
			}else if noDrop && !tcount {
				log.Info("原来在 im 中需要移到 re中", "nodeId", nodeId.String())
				if delIds, canArr, err := handleFunc("Immediate", "Reserve", nodeId, c.immediateCandidates, c.reserveCandidates,
					c.delImmediate, c.delReserve, c.setReserve, c.getImmediateIndex, c.setImmediateIndex, c.setReserveIndex); nil != err {
					return nil, err
				} else {
					c.reserveCacheArr = append(c.reserveCacheArr, canArr...)
					if len(delIds) != 0 {
						delNodeIds = append(delNodeIds, delIds...)
					}
				}
			}
		case IS_RESERVE:
			log.Debug("当前nodeId原来在 Reserve 中...")
			can := c.reserveCandidates[nodeId]
			//if c.checkTicket(state.TCount(nodeId)) { // TODO
			if tcount, noDrop := c.checkDeposit(state, can); !noDrop { // 直接掉榜
				if err := directdropFunc("Reserve", can, c.delReserve, c.getReserveIndex, c.setReserveIndex); nil != err {
					return nil, err
				}else {
					delNodeIds = append(delNodeIds, nodeId)
				}
			}else if noDrop && tcount{
				log.Info("原来在 re 中需要移到 im中", "nodeId", nodeId.String())
				if delIds, canArr, err := handleFunc("Reserve", "Immediate", nodeId, c.reserveCandidates, c.immediateCandidates,
					c.delReserve, c.delImmediate, c.setImmediate, c.getReserveIndex, c.setReserveIndex, c.setImmediateIndex); nil != err {
					return nil, err
				} else {
					c.immediateCacheArr = append(c.immediateCacheArr, canArr...)
					if len(delIds) != 0 {
						delNodeIds = append(delNodeIds, delIds...)
					}
				}
			}
		default:
			continue
		}
	}

	return delNodeIds, nil
}

// 揭榜后重新质押前的操作
// false: direct get out
// true: pass
func (c *CandidatePool) preElectionReset(state vm.StateDB, can *types.Candidate) (bool, error) {
	// 如果校验不通过的话，则直接掉榜，但掉榜前需要判断之前是在哪个队列的
	if _, ok := c.checkDeposit(state, can); !ok {
		log.Warn("Failed to checkDeposit on preElectionReset", "nodeId", can.CandidateId.String(), " err", DepositLowErr)
		var del int // del: 1 del immiedate; 2  del reserve
		if _, ok := c.immediateCandidates[can.CandidateId]; ok {
			del = IS_IMMEDIATE
		}
		if _, ok := c.reserveCandidates[can.CandidateId]; ok {
			del = IS_RESERVE
		}

		// 直接产生退款信息
		delFunc := func(title string, delInfoFn func(state vm.StateDB, candidateId discover.NodeID),
			getIndexFn func(state vm.StateDB) ([]discover.NodeID, error),
			setIndexFn func(state vm.StateDB, nodeIds []discover.NodeID) error) error {

			log.Debug("进入直接掉榜逻辑,从" + title + ",中删除...")
			delInfoFn(state, can.CandidateId)

			// append to refunds (defeat) trie
			if err := c.setDefeat(state, can.CandidateId, can); nil != err {
				return err
			}

			// update index of refund (defeat) on trie
			if err := c.setDefeatIndex(state); nil != err {
				return err
			}

			// update reserve id index
			if ids, err := getIndexFn(state); nil != err {
				log.Error("withdraw failed get"+title+"Index on preElectionReset", "err", err)
				return err
			} else {
				//for i, id := range ids {
				for i := 0; i < len(ids); i++ {
					id := ids[i]
					if id == can.CandidateId {
						ids = append(ids[:i], ids[i+1:]...)
						i--
					}
				}
				if err := setIndexFn(state, ids); nil != err {
					log.Error("withdraw failed set"+title+"Index on preElectionReset", "err", err)
					return err
				}
			}
			return nil
		}

		if del == IS_IMMEDIATE {
			/** first delete this can on immediates */
			return false, delFunc("Immediate", c.delImmediate, c.getImmediateIndex, c.setImmediateIndex)
		} else {
			/** first delete this can on reserves */
			return false, delFunc("Reserve", c.delReserve, c.getReserveIndex, c.setReserveIndex)
		}
	}
	return true, nil
}

func (c *CandidatePool) checkFirstThreshold (can *types.Candidate) (bool) {
	var exist bool
	if _, ok := c.immediateCandidates[can.CandidateId]; ok {
		exist = true
	}

	if _, ok := c.reserveCandidates[can.CandidateId]; ok {
		exist = true
	}

	if !exist && can.Deposit.Cmp(c.threshold) < 0 {
		return false
	}
	return true
}

// false: invalid deposit
// true:  pass
func (c *CandidatePool) checkDeposit(state vm.StateDB, can *types.Candidate) (bool, bool) {
	tcount := c.checkTicket(state.TCount(can.CandidateId))
	/**
	如果 当前得票数满足进入候选池，且候选池已满
	 */
	if tcount && uint64(len(c.immediateCandidates)) == c.maxCount {
		last := c.immediateCacheArr[len(c.immediateCacheArr)-1]
		lastDeposit := last.Deposit

		// y = 100 + x
		percentage := new(big.Int).Add(big.NewInt(100), big.NewInt(int64(c.depositLimit)))
		// z = old * y
		tmp := new(big.Int).Mul(lastDeposit, percentage)
		// z/100 == old * (100 + x) / 100 == old * (y%)
		tmp = new(big.Int).Div(tmp, big.NewInt(100))
		if can.Deposit.Cmp(tmp) < 0 {
			log.Debug("候选池已满，且当前can的Deposit 小于 候选池中最后一个can的 110% ", "当前 can的Deposit:", can.Deposit.String(), "候选池中最后一个can的 110%:", tmp.String(), "当前候选池长度:", len(c.immediateCandidates), "配置上限:", c.maxCount)
			return tcount, false
		}
	}

	/**
	如果 当前不如 候选池 且备选池已满
	 */
	if !tcount && uint64(len(c.reserveCandidates)) == c.maxCount {
		last := c.reserveCacheArr[len(c.reserveCacheArr)-1]
		lastDeposit := last.Deposit

		// y = 100 + x
		percentage := new(big.Int).Add(big.NewInt(100), big.NewInt(int64(c.depositLimit)))
		// z = old * y
		tmp := new(big.Int).Mul(lastDeposit, percentage)
		// z/100 == old * (100 + x) / 100 == old * (y%)
		tmp = new(big.Int).Div(tmp, big.NewInt(100))
		if can.Deposit.Cmp(tmp) < 0 {
			log.Debug("备选池已满，且当前can的Deposit 小于 备选池中最后一个can的 110% ", "当前 can的Deposit:", can.Deposit.String(), "备选池总中最后一个can的 110%:", tmp.String(), "当前备选池长度:", len(c.reserveCandidates), "配置上限:", c.maxCount)
			return tcount, false
		}
	}
	return tcount, true
}

func (c *CandidatePool) checkWithdraw(source, price *big.Int) error {
	// y = old * x
	percentage := new(big.Int).Mul(source, big.NewInt(int64(c.depositLimit)))
	// y/100 == old * (x/100) == old * x%
	tmp := new(big.Int).Div(percentage, big.NewInt(100))
	if price.Cmp(tmp) < 0 {
		log.Debug("提取退款,当前can的想退的金额小于自身质押剩余金额的 10%:", "can的当前想退的金额:", price.String(), "can自身质押剩余的金额:", tmp.String())
		return WithdrawLowErr
	}
	return nil
}

// 0: empty
// 1: in immediates
// 2: in reserves
func (c *CandidatePool) checkExist(nodeId discover.NodeID) int {
	log.Info("判断当前nodeId原来属于哪个队列", "nodeId", nodeId.String())
	if _, ok := c.immediateCandidates[nodeId]; ok {
		log.Info("判断当前nodeId原来属于 immediate 队列 ...", "nodeId", nodeId.String())
		return IS_IMMEDIATE
	}
	if _, ok := c.reserveCandidates[nodeId]; ok {
		log.Info("判断当前nodeId原来属于 reserve 队列 ...", "nodeId", nodeId.String())
		return IS_RESERVE
	}
	log.Info("判断当前nodeId原来不属于任意队列 ...", "nodeId", nodeId.String())
	return IS_LOST
}

func (c *CandidatePool) checkTicket(t_count uint64) bool {
	log.Info("对比当前候选人得票数为:", "t_count", t_count, "入选门槛为:", c.allowed)
	if t_count >= c.allowed {
		log.Info("当前候选人得票数符合进入候选池...")
		return true
	}
	log.Info("不进候选池...")
	return false
}

func (c *CandidatePool) setImmediate(state vm.StateDB, candidateId discover.NodeID, can *types.Candidate) error {
	c.immediateCandidates[candidateId] = can
	if value, err := rlp.EncodeToBytes(can); nil != err {
		log.Error("Failed to encode candidate object on setImmediate", "key", candidateId.String(), "err", err)
		return CandidateEncodeErr
	} else {
		// set immediate candidate input the trie
		setImmediateState(state, candidateId, value)
	}
	return nil
}

func (c *CandidatePool) getImmediateIndex(state vm.StateDB) ([]discover.NodeID, error) {
	return getImmediateIdsByState(state)
}

// deleted immediate candidate by nodeId (Automatically update the index)
func (c *CandidatePool) delImmediate(state vm.StateDB, candidateId discover.NodeID) {
	//// deleted immediate candidate by id on trie
	//setImmediateState(state, candidateId, []byte{})
	//// deleted immedidate candidate by id on map
	//delete(c.immediateCandidates, candidateId)
}

func (c *CandidatePool) setImmediateIndex(state vm.StateDB, nodeIds []discover.NodeID) error {
	if len(nodeIds) == 0 {
		//setImmediateIdsState(state, []byte{})
		return nil
	}
	if val, err := rlp.EncodeToBytes(nodeIds); nil != err {
		log.Error("Failed to encode ImmediateIds", "err", err)
		return err
	} else {
		setImmediateIdsState(state, val)
	}
	return nil
}

func (c *CandidatePool) setReserve(state vm.StateDB, candidateId discover.NodeID, can *types.Candidate) error {
	c.reserveCandidates[candidateId] = can
	if value, err := rlp.EncodeToBytes(can); nil != err {
		log.Error("Failed to encode candidate object on setReserve", "key", candidateId.String(), "err", err)
		return CandidateEncodeErr
	} else {
		// set setReserve candidate input the trie
		setReserveState(state, candidateId, value)
	}
	return nil
}

// deleted reserve candidate by nodeId (Automatically update the index)
func (c *CandidatePool) delReserve(state vm.StateDB, candidateId discover.NodeID) {
	//// deleted reserve candidate by id on trie
	//setReserveState(state, candidateId, []byte{})
	//// deleted reserve candidate by id on map
	//delete(c.reserveCandidates, candidateId)
}

func (c *CandidatePool) getReserveIndex(state vm.StateDB) ([]discover.NodeID, error) {
	return getReserveIdsByState(state)
}

func (c *CandidatePool) setReserveIndex(state vm.StateDB, nodeIds []discover.NodeID) error {
	if len(nodeIds) == 0 {
		//setReserveIdsState(state, []byte{})
		return nil
	}
	if val, err := rlp.EncodeToBytes(nodeIds); nil != err {
		log.Error("Failed to encode ReserveIds", "err", err)
		return err
	} else {
		setReserveIdsState(state, val)
	}
	return nil
}

// setting refund information
func (c *CandidatePool) setDefeat(state vm.StateDB, candidateId discover.NodeID, can *types.Candidate) error {

	var defeatArr types.CandidateQueue
	// append refund information
	if defeatArrTmp, ok := c.defeatCandidates[can.CandidateId]; ok {
		defeatArrTmp = append(defeatArrTmp, can)
		//c.defeatCandidates[can.CandidateId] = defeatArrTmp
		defeatArr = defeatArrTmp
	} else {
		defeatArrTmp = make(types.CandidateQueue, 0)
		defeatArrTmp = append(defeatArr, can)
		//c.defeatCandidates[can.CandidateId] = defeatArrTmp
		defeatArr = defeatArrTmp
	}
	// setting refund information on trie
	if value, err := rlp.EncodeToBytes(&defeatArr); nil != err {
		log.Error("Failed to encode candidate object on setDefeat", "key", candidateId.String(), "err", err)
		return CandidateEncodeErr
	} else {
		setDefeatState(state, candidateId, value)
		c.defeatCandidates[can.CandidateId] = defeatArr
	}
	return nil
}

func (c *CandidatePool) delDefeat(state vm.StateDB, nodeId discover.NodeID) {
	//delete(c.defeatCandidates, nodeId)
	//setDefeatState(state, nodeId, []byte{})
}

// update refund index
func (c *CandidatePool) setDefeatIndex(state vm.StateDB) error {
	newdefeatIds := make([]discover.NodeID, 0)
	indexMap := make(map[string]discover.NodeID, 0)
	index := make([]string, 0)
	for id, _ := range c.defeatCandidates {
		indexMap[id.String()] = id
		index = append(index, id.String())
	}
	// sort id
	sort.Strings(index)

	for _, idStr := range index {
		id := indexMap[idStr]
		newdefeatIds = append(newdefeatIds, id)
	}
	if len(newdefeatIds) == 0 {
		//setDefeatIdsState(state, []byte{})
		return nil
	}
	if value, err := rlp.EncodeToBytes(&newdefeatIds); nil != err {
		log.Error("Failed to encode candidate object on setDefeatIds", "err", err)
		return CandidateEncodeErr
	} else {
		setDefeatIdsState(state, value)
	}
	return nil
}

func (c *CandidatePool) delPreviousWitness(state vm.StateDB, candidateId discover.NodeID) {
	//// deleted previous witness by id on map
	//delete(c.preOriginCandidates, candidateId)
	//// delete previous witness by id on trie
	//setPreviousWitnessState(state, candidateId, []byte{})
}

func (c *CandidatePool) setPreviousWitness(state vm.StateDB, nodeId discover.NodeID, can *types.Candidate) error {
	c.preOriginCandidates[nodeId] = can
	if val, err := rlp.EncodeToBytes(can); nil != err {
		log.Error("Failed to encode Candidate on setPreviousWitness", "err", err)
		return err
	} else {
		setPreviousWitnessState(state, nodeId, val)
	}
	return nil
}

func (c *CandidatePool) setPreviousWitnessindex(state vm.StateDB, nodeIds []discover.NodeID) error {
	if len(nodeIds) == 0 {
		//setPreviosWitnessIdsState(state, []byte{})
		return nil
	}
	if val, err := rlp.EncodeToBytes(nodeIds); nil != err {
		log.Error("Failed to encode Previous WitnessIds", "err", err)
		return err
	} else {
		setPreviosWitnessIdsState(state, val)
	}
	return nil
}

func (c *CandidatePool) getPreviousWitnessIndex(state vm.StateDB) ([]discover.NodeID, error) {
	return getPreviousWitnessIdsState(state)
}

func (c *CandidatePool) setWitness(state vm.StateDB, nodeId discover.NodeID, can *types.Candidate) error {
	c.originCandidates[nodeId] = can
	if val, err := rlp.EncodeToBytes(can); nil != err {
		log.Error("Failed to encode Candidate on setWitness", "err", err)
		return err
	} else {
		setWitnessState(state, nodeId, val)
	}
	return nil
}

func (c *CandidatePool) setWitnessindex(state vm.StateDB, nodeIds []discover.NodeID) error {
	if len(nodeIds) == 0 {
		//setWitnessIdsState(state, []byte{})
		return nil
	}
	if val, err := rlp.EncodeToBytes(nodeIds); nil != err {
		log.Error("Failed to encode WitnessIds", "err", err)
		return err
	} else {
		setWitnessIdsState(state, val)
	}
	return nil
}

func (c *CandidatePool) delWitness(state vm.StateDB, candidateId discover.NodeID) {
	//// deleted witness by id on map
	//delete(c.originCandidates, candidateId)
	//// delete witness by id on trie
	//setWitnessState(state, candidateId, []byte{})
}

func (c *CandidatePool) getWitnessIndex(state vm.StateDB) ([]discover.NodeID, error) {
	return getWitnessIdsByState(state)
}

func (c *CandidatePool) setNextWitness(state vm.StateDB, nodeId discover.NodeID, can *types.Candidate) error {
	c.nextOriginCandidates[nodeId] = can
	if value, err := rlp.EncodeToBytes(can); nil != err {
		log.Error("Failed to encode candidate object on setImmediate", "key", nodeId.String(), "err", err)
		return CandidateEncodeErr
	} else {
		// setting next witness information on trie
		setNextWitnessState(state, nodeId, value)
	}
	return nil
}

func (c *CandidatePool) delNextWitness(state vm.StateDB, candidateId discover.NodeID) {
	//// deleted next witness by id on map
	//delete(c.nextOriginCandidates, candidateId)
	//// deleted next witness by id on trie
	//setNextWitnessState(state, candidateId, []byte{})
}

func (c *CandidatePool) setNextWitnessIndex(state vm.StateDB, nodeIds []discover.NodeID) error {
	if len(nodeIds) == 0 {
		//setNextWitnessIdsState(state, []byte{})
		return nil
	}
	if value, err := rlp.EncodeToBytes(&nodeIds); nil != err {
		log.Error("Failed to encode candidate object on setDefeatIds", "err", err)
		return CandidateEncodeErr
	} else {
		setNextWitnessIdsState(state, value)
	}
	return nil
}

func (c *CandidatePool) getNextWitnessIndex(state vm.StateDB) ([]discover.NodeID, error) {
	return getNextWitnessIdsByState(state)
}

func (c *CandidatePool) getCandidate(state vm.StateDB, nodeId discover.NodeID) (*types.Candidate, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on getCandidate", "err", err)
		return nil, err
	}
	if candidatePtr, ok := c.immediateCandidates[nodeId]; ok {
		PrintObject("Call GetCandidate return immediate：", *candidatePtr)
		return candidatePtr, nil
	}
	if candidatePtr, ok := c.reserveCandidates[nodeId]; ok {
		PrintObject("Call GetCandidate return reserve：", *candidatePtr)
		return candidatePtr, nil
	}
	return nil, nil
}

func (c *CandidatePool) getCandidates(state vm.StateDB, nodeIds ...discover.NodeID) (types.CandidateQueue, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if err := c.initDataByState(state, 1); nil != err {
		log.Error("Failed to initDataByState on getCandidates", "err", err)
		return nil, err
	}

	canArr := make(types.CandidateQueue, 0)
	tem := make(map[discover.NodeID]struct{}, 0)
	for _, nodeId := range nodeIds {
		if _, ok := tem[nodeId]; ok {
			continue
		}
		if candidatePtr, ok := c.immediateCandidates[nodeId]; ok {
			canArr = append(canArr, candidatePtr)
			tem[nodeId] = struct{}{}
		}
		if _, ok := tem[nodeId]; ok {
			continue
		}
		if candidatePtr, ok := c.reserveCandidates[nodeId]; ok {
			canArr = append(canArr, candidatePtr)
			tem[nodeId] = struct{}{}
		}
	}
	return canArr, nil
}

func (c *CandidatePool) MaxChair() uint64 {
	return c.maxChair
}

func (c *CandidatePool) MaxCount() uint64 {
	return c.maxCount
}

func getPreviousWitnessIdsState(state vm.StateDB) ([]discover.NodeID, error) {
	var witnessIds []discover.NodeID
	if valByte := state.GetState(common.CandidatePoolAddr, PreviousWitnessListKey()); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &witnessIds); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return witnessIds, nil
}

func setPreviosWitnessIdsState(state vm.StateDB, arrVal []byte) {
	state.SetState(common.CandidatePoolAddr, PreviousWitnessListKey(), arrVal)
}

func getPreviousWitnessByState(state vm.StateDB, id discover.NodeID) (*types.Candidate, error) {
	var can types.Candidate
	if valByte := state.GetState(common.CandidatePoolAddr, PreviousWitnessKey(id)); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &can); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return &can, nil
}

func setPreviousWitnessState(state vm.StateDB, id discover.NodeID, val []byte) {
	state.SetState(common.CandidatePoolAddr, PreviousWitnessKey(id), val)
}

func getWitnessIdsByState(state vm.StateDB) ([]discover.NodeID, error) {
	var witnessIds []discover.NodeID
	if valByte := state.GetState(common.CandidatePoolAddr, WitnessListKey()); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &witnessIds); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return witnessIds, nil
}

func setWitnessIdsState(state vm.StateDB, arrVal []byte) {
	state.SetState(common.CandidatePoolAddr, WitnessListKey(), arrVal)
}

func getWitnessByState(state vm.StateDB, id discover.NodeID) (*types.Candidate, error) {
	var can types.Candidate
	if valByte := state.GetState(common.CandidatePoolAddr, WitnessKey(id)); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &can); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return &can, nil
}

func setWitnessState(state vm.StateDB, id discover.NodeID, val []byte) {
	state.SetState(common.CandidatePoolAddr, WitnessKey(id), val)
}

func getNextWitnessIdsByState(state vm.StateDB) ([]discover.NodeID, error) {
	var nextWitnessIds []discover.NodeID
	if valByte := state.GetState(common.CandidatePoolAddr, NextWitnessListKey()); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &nextWitnessIds); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return nextWitnessIds, nil
}

func setNextWitnessIdsState(state vm.StateDB, arrVal []byte) {
	state.SetState(common.CandidatePoolAddr, NextWitnessListKey(), arrVal)
}

func getNextWitnessByState(state vm.StateDB, id discover.NodeID) (*types.Candidate, error) {
	var can types.Candidate
	if valByte := state.GetState(common.CandidatePoolAddr, NextWitnessKey(id)); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &can); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return &can, nil
}

func setNextWitnessState(state vm.StateDB, id discover.NodeID, val []byte) {
	state.SetState(common.CandidatePoolAddr, NextWitnessKey(id), val)
}

func getImmediateIdsByState(state vm.StateDB) ([]discover.NodeID, error) {
	var immediateIds []discover.NodeID
	if valByte := state.GetState(common.CandidatePoolAddr, ImmediateListKey()); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &immediateIds); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return immediateIds, nil
}

func setImmediateIdsState(state vm.StateDB, arrVal []byte) {
	state.SetState(common.CandidatePoolAddr, ImmediateListKey(), arrVal)
}

func getImmediateByState(state vm.StateDB, id discover.NodeID) (*types.Candidate, error) {
	var can types.Candidate
	if valByte := state.GetState(common.CandidatePoolAddr, ImmediateKey(id)); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &can); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return &can, nil
}

func setImmediateState(state vm.StateDB, id discover.NodeID, val []byte) {
	state.SetState(common.CandidatePoolAddr, ImmediateKey(id), val)
}

func getReserveIdsByState(state vm.StateDB) ([]discover.NodeID, error) {
	var reserveIds []discover.NodeID
	if valByte := state.GetState(common.CandidatePoolAddr, ReserveListKey()); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &reserveIds); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return reserveIds, nil
}

func setReserveIdsState(state vm.StateDB, arrVal []byte) {
	state.SetState(common.CandidatePoolAddr, ReserveListKey(), arrVal)
}

func getReserveByState(state vm.StateDB, id discover.NodeID) (*types.Candidate, error) {
	var can types.Candidate
	if valByte := state.GetState(common.CandidatePoolAddr, ReserveKey(id)); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &can); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return &can, nil
}

func setReserveState(state vm.StateDB, id discover.NodeID, val []byte) {
	state.SetState(common.CandidatePoolAddr, ReserveKey(id), val)
}

func getDefeatIdsByState(state vm.StateDB) ([]discover.NodeID, error) {
	var defeatIds []discover.NodeID
	if valByte := state.GetState(common.CandidatePoolAddr, DefeatListKey()); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &defeatIds); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return defeatIds, nil
}

func setDefeatIdsState(state vm.StateDB, arrVal []byte) {
	state.SetState(common.CandidatePoolAddr, DefeatListKey(), arrVal)
}

func getDefeatsByState(state vm.StateDB, id discover.NodeID) (types.CandidateQueue, error) {
	var canArr types.CandidateQueue
	if valByte := state.GetState(common.CandidatePoolAddr, DefeatKey(id)); len(valByte) != 0 {
		if err := rlp.DecodeBytes(valByte, &canArr); nil != err {
			return nil, err
		}
	}else {
		return nil, nil
	}
	return canArr, nil
}

func setDefeatState(state vm.StateDB, id discover.NodeID, val []byte) {
	state.SetState(common.CandidatePoolAddr, DefeatKey(id), val)
}

func copyCandidateMapByIds(target, source candidateStorage, ids []discover.NodeID) {
	for _, id := range ids {
		if v, ok := source[id]; ok {
			target[id] = v
		}
	}
}

//func GetCandidatePtr() *CandidatePool {
//	return candidatePool
//}

func PrintObject(s string, obj interface{}) {
	objs, _ := json.Marshal(obj)

	log.Debug(s, "==", string(objs))
	//fmt.Println(s, string(objs))
}

func buildWitnessNode(can *types.Candidate) (*discover.Node, error) {
	if nil == can {
		return nil, CandidateEmptyErr
	}
	ip := net.ParseIP(can.Host)
	// uint16
	var port uint16
	if portInt, err := strconv.Atoi(can.Port); nil != err {
		return nil, err
	} else {
		port = uint16(portInt)
	}
	return discover.NewNode(can.CandidateId, ip, port, port), nil
}


func  makeCandidateSort(state vm.StateDB, arr types.CandidateQueue) {
	cand := make(types.CanConditions, 0)
	for _, can := range arr {
		tCount := state.TCount(can.CandidateId)
		tprice := new(big.Int).Mul(big.NewInt(int64(tCount)), ticketPool.TicketPrice)

		money := new(big.Int).Add(can.Deposit, tprice)

		cand[can.CandidateId] = money
	}
	arr.CandidateSort(cand)
}


//func compare(c, can *types.Candidate) int {
//	// put the larger deposit in front
//	if c.Deposit.Cmp(can.Deposit) > 0 {
//		return 1
//	} else if c.Deposit.Cmp(can.Deposit) == 0 {
//		// put the smaller blocknumber in front
//		if c.BlockNumber.Cmp(can.BlockNumber) > 0 {
//			return -1
//		} else if c.BlockNumber.Cmp(can.BlockNumber) == 0 {
//			// put the smaller tx'index in front
//			if c.TxIndex > can.TxIndex {
//				return -1
//			} else if c.TxIndex == can.TxIndex {
//				return 0
//			} else {
//				return 1
//			}
//		} else {
//			return 1
//		}
//	} else {
//		return -1
//	}
//}
//
//// sorted candidates
//func candidateSort(arr types.CandidateQueue) {
//	if len(arr) <= 1 {
//		return
//	}
//	quickSort(arr, 0, len(arr)-1)
//}
//func quickSort(arr types.CandidateQueue, left, right int) {
//	if left < right {
//		pivot := partition(arr, left, right)
//		quickSort(arr, left, pivot-1)
//		quickSort(arr, pivot+1, right)
//	}
//}
//func partition(arr types.CandidateQueue, left, right int) int {
//	for left < right {
//		for left < right && compare(arr[left], arr[right]) >= 0 {
//			right--
//		}
//		if left < right {
//			arr[left], arr[right] = arr[right], arr[left]
//			left++
//		}
//		for left < right && compare(arr[left], arr[right]) >= 0 {
//			left++
//		}
//		if left < right {
//			arr[left], arr[right] = arr[right], arr[left]
//			right--
//		}
//	}
//	return left
//}

func ImmediateKey(nodeId discover.NodeID) []byte {
	return immediateKey(nodeId.Bytes())
}
func immediateKey(key []byte) []byte {
	return append(ImmediateBytePrefix, key...)
}

func ReserveKey(nodeId discover.NodeID) []byte {
	return reserveKey(nodeId.Bytes())
}

func reserveKey(key []byte) []byte {
	return append(ReserveBytePrefix, key...)
}

func PreviousWitnessKey(nodeId discover.NodeID) []byte {
	return prewitnessKey(nodeId.Bytes())
}

func prewitnessKey(key []byte) []byte {
	return append(PreWitnessBytePrefix, key...)
}

func WitnessKey(nodeId discover.NodeID) []byte {
	return witnessKey(nodeId.Bytes())
}
func witnessKey(key []byte) []byte {
	return append(WitnessBytePrefix, key...)
}

func NextWitnessKey(nodeId discover.NodeID) []byte {
	return nextWitnessKey(nodeId.Bytes())
}
func nextWitnessKey(key []byte) []byte {
	return append(NextWitnessBytePrefix, key...)
}

func DefeatKey(nodeId discover.NodeID) []byte {
	return defeatKey(nodeId.Bytes())
}
func defeatKey(key []byte) []byte {
	return append(DefeatBytePrefix, key...)
}

func ImmediateListKey() []byte {
	return ImmediateListBytePrefix
}

func ReserveListKey() []byte {
	return ReserveListBytePrefix
}

func PreviousWitnessListKey() []byte {
	return PreWitnessListBytePrefix
}

func WitnessListKey() []byte {
	return WitnessListBytePrefix
}

func NextWitnessListKey() []byte {
	return NextWitnessListBytePrefix
}

func DefeatListKey() []byte {
	return DefeatListBytePrefix
}

func (c *CandidatePool) ForEachStorage (state vm.StateDB, title string) {
	c.lock.Lock()
	log.Debug(title + ":完全查看候选池中的数据 ...")
	c.initDataByState(state, 2)
	c.lock.Unlock()
}