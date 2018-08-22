package services

import (
	"time"

	"sync"

	"github.com/btcsuite/goleveldb/leveldb/errors"
	"github.com/coreos/bbolt"
	"github.com/dedis/cothority/skipchain"
	"github.com/dedis/kyber/util/random"
	"github.com/dedis/onet"
	"github.com/dedis/onet/log"
	"github.com/dedis/onet/network"
	"github.com/fanliao/go-concurrentMap"
	"github.com/lca1/unlynx/lib"
	"github.com/lca1/unlynx/protocols"
	"github.com/lca1/unlynx/skip"
	"github.com/lca1/drynx/lib"
	"github.com/lca1/drynx/lib/proof"
	"github.com/lca1/drynx/protocols"
)

// ServiceName is the registered name for the lemal service.
const ServiceName = "lemal"

const gobFile = "pre_compute_multiplications.gob"

// Survey represents a survey with the corresponding params
type Survey struct {
	SurveyQuery        lib.SurveyQuery
	QueryResponseState libunlynx.ResponseAllDPs // QueryResponse keeps track of the response from the data providers, the aggregated data, and the final results
	Noises             libunlynx.CipherVector
	ShufflePrecompute  []libunlynx.CipherVectorScalar
	MapPIs             map[string]onet.ProtocolInstance

	// channels
	DPqueryChannel chan int // To wait for all DPs to finish the getting the query before continuing
	SyncDCPChannel chan int // To wait to synchronize the execution of the data collection protocol between the servers
	DPdataChannel  chan int // To wait for all nodes to finish the getting their data before continuing
	DiffPChannel   chan int // To wait for the noise to be collectively computed

	// mutex
	Mutex *sync.Mutex
}

func castToSurvey(object interface{}, err error) Survey {
	if err != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Error reading map")
	}
	return object.(Survey)
}

// DPqueryReceived is used to ensure that all DPs have received the query and can proceed with the data collection protocol
type DPqueryReceived struct {
	SurveyID string
}

type SyncDCP struct {
	SurveyID string
}

// DPdataFinished is used to ensure that all servers have received the data and can proceed with the collective aggregation
type DPdataFinished struct {
	SurveyID string
}

// Service defines a service in unlynx with a survey.
type ServiceLeMal struct {
	*onet.ServiceProcessor

	// ---- Computing Nodes ----
	Survey *concurrent.ConcurrentMap
	// -------------------------

	// ---- Verifying Nodes ----
	Skipchain     *skipchain.Client
	LastSkipBlock *skipchain.SkipBlock
	//Contains size for a query, as well as the bitmap for this query
	Request *concurrent.ConcurrentMap
	//the name of DB and the DB in itself is dedicated to the server.
	DBPath string
	DB     *bolt.DB
	//To make everything thread safe (database access and updating parameters)
	Mutex *sync.Mutex
	// Too receive the bitmaps from the nodes
	SharedBMChannel chan map[string]int64
	// To finish the protocols
	SharedBMChannelToTerminate chan struct{}
	// -------------------------
}

// MsgTypes defines the Message Type SurveyID for all the service's intra-messages.
type MsgTypes struct {
	msgSurveyQuery     network.MessageTypeID
	msgSurveyQueryToDP network.MessageTypeID
	msgDPqueryReceived network.MessageTypeID
	msgSyncDCP         network.MessageTypeID
	msgDPdataFinished  network.MessageTypeID
}

var msgTypes = MsgTypes{}

func init() {
	onet.RegisterNewService(ServiceName, NewService)

	msgTypes.msgSurveyQuery = network.RegisterMessage(&lib.SurveyQuery{})
	msgTypes.msgSurveyQueryToDP = network.RegisterMessage(&lib.SurveyQueryToDP{})
	msgTypes.msgDPqueryReceived = network.RegisterMessage(&DPqueryReceived{})
	msgTypes.msgSyncDCP = network.RegisterMessage(&SyncDCP{})
	msgTypes.msgDPdataFinished = network.RegisterMessage(&DPdataFinished{})

	network.RegisterMessage(&lib.SurveyQueryToVN{})
	network.RegisterMessage(&lib.ResponseDP{})

	network.RegisterMessage(&lib.EndVerificationRequest{})

	network.RegisterMessage(skip.DataBlock{})
	network.RegisterMessage(&lib.GetLatestBlock{})
	network.RegisterMessage(&lib.GetGenesis{})
	network.RegisterMessage(&lib.GetBlock{})
	network.RegisterMessage(&lib.GetProofs{})
	network.RegisterMessage(&lib.CloseDB{})
}

// NewService constructor which registers the needed messages.
func NewService(c *onet.Context) (onet.Service, error) {
	newUnLynxInstance := &ServiceLeMal{
		ServiceProcessor: onet.NewServiceProcessor(c),
		Survey:           concurrent.NewConcurrentMap(),
		Mutex:            &sync.Mutex{},
	}
	var cerr error
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleSurveyQuery); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleSurveyQueryToDP); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleSurveyQueryToVN); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleDPqueryReceived); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleSyncDCP); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleDPdataFinished); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleEndVerification); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleGetLatestBlock); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleGetGenesis); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleGetBlock); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleGetProofs); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}
	if cerr = newUnLynxInstance.RegisterHandler(newUnLynxInstance.HandleCloseDB); cerr != nil {
		log.Fatal("[SERVICE] <LEMAL> Server, Wrong Handler.", cerr)
	}

	c.RegisterProcessor(newUnLynxInstance, msgTypes.msgSurveyQuery)
	c.RegisterProcessor(newUnLynxInstance, msgTypes.msgSurveyQueryToDP)
	c.RegisterProcessor(newUnLynxInstance, msgTypes.msgDPqueryReceived)
	c.RegisterProcessor(newUnLynxInstance, msgTypes.msgSyncDCP)
	c.RegisterProcessor(newUnLynxInstance, msgTypes.msgDPdataFinished)

	//Register new verifFunction
	if err := skipchain.RegisterVerification(c, VerifyBitmap, newUnLynxInstance.verifyFuncBitmap); err != nil {
		return nil, err
	}

	return newUnLynxInstance, cerr
}

// Process implements the processor interface and is used to recognize messages broadcasted between servers
func (s *ServiceLeMal) Process(msg *network.Envelope) {
	if msg.MsgType.Equal(msgTypes.msgSurveyQuery) {
		tmp := (msg.Msg).(*lib.SurveyQuery)
		s.HandleSurveyQuery(tmp)
	} else if msg.MsgType.Equal(msgTypes.msgSurveyQueryToDP) {
		tmp := (msg.Msg).(*lib.SurveyQueryToDP)
		s.HandleSurveyQueryToDP(tmp)
	} else if msg.MsgType.Equal(msgTypes.msgDPqueryReceived) {
		tmp := (msg.Msg).(*DPqueryReceived)
		s.HandleDPqueryReceived(tmp)
	} else if msg.MsgType.Equal(msgTypes.msgSyncDCP) {
		tmp := (msg.Msg).(*SyncDCP)
		s.HandleSyncDCP(tmp)
	} else if msg.MsgType.Equal(msgTypes.msgDPdataFinished) {
		tmp := (msg.Msg).(*DPdataFinished)
		s.HandleDPdataFinished(tmp)
	}
}

// Query Handlers
//______________________________________________________________________________________________________________________

// HandleDPqueryReceived handles the channel that each server has to know when to proceed with data collection protocol
func (s *ServiceLeMal) HandleDPqueryReceived(recq *DPqueryReceived) (network.Message, error) {
	var el interface{}
	el = nil
	for el == nil {
		el, _ = s.Survey.Get(recq.SurveyID)

		if el != nil {
			break
		}

		time.Sleep(time.Millisecond * 100)
	}

	castToSurvey(s.Survey.Get(recq.SurveyID)).DPqueryChannel <- 1
	return nil, nil
}

// HandleDPqueryReceived handles the channel that each server has to know when to proceed with data collection protocol
func (s *ServiceLeMal) HandleSyncDCP(recq *SyncDCP) (network.Message, error) {
	var el interface{}
	el = nil
	for el == nil {
		el, _ = s.Survey.Get(recq.SurveyID)

		if el != nil {
			break
		}

		time.Sleep(time.Millisecond * 100)
	}

	castToSurvey(s.Survey.Get(recq.SurveyID)).SyncDCPChannel <- 1
	return nil, nil
}

// HandleDPdataFinished handles the channel that each server has to know when to proceed with the collective aggregation
func (s *ServiceLeMal) HandleDPdataFinished(recq *DPdataFinished) (network.Message, error) {
	var el interface{}
	el = nil
	for el == nil {
		el, _ = s.Survey.Get(recq.SurveyID)

		if el != nil {
			break
		}

		time.Sleep(time.Millisecond * 100)
	}

	castToSurvey(s.Survey.Get(recq.SurveyID)).DPdataChannel <- 1
	return nil, nil
}

// HandleSurveyQuery handles the reception of a survey creation query by instantiating the corresponding survey.
func (s *ServiceLeMal) HandleSurveyQuery(recq *lib.SurveyQuery) (network.Message, error) {

	log.Lvl2("[SERVICE] <LEMAL> Server", s.ServerIdentity().String(), "received a Survey Query")

	recq.Query.IVSigs.InputValidationSigs = recreateRangeSignatures(recq.Query.IVSigs)

	// get the total number DPs
	nbrDPs := 0
	for _, v := range recq.ServerToDP {
		if v != nil {
			nbrDPs += len(*v)
		}
	}

	// only generate ProofCollection protocol instances if proofs is enabled
	var mapPIs map[string]onet.ProtocolInstance
	if recq.Query.Proofs != 0 {
		mapPIs = s.generateMapPIs(recq)
	}

	// survey instantiation
	s.Survey.Put(recq.SurveyID, Survey{
		SurveyQuery:    *recq,
		DPqueryChannel: make(chan int, nbrDPs),
		SyncDCPChannel: make(chan int, nbrDPs),
		DPdataChannel:  make(chan int, nbrDPs),
		DiffPChannel:   make(chan int, nbrDPs),
		MapPIs:         mapPIs,
	})

	survey := castToSurvey(s.Survey.Get(recq.SurveyID))

	// prepares the precomputation for shuffling
	lineSize := 100 // + 1 is for the possible count attribute
	survey.ShufflePrecompute = libunlynx.PrecomputationWritingForShuffling(false, gobFile, s.ServerIdentity().String(), libunlynx.SuiTe.Scalar().Pick(random.New()), recq.RosterServers.Aggregate, lineSize)

	// if is the root server: send query to all other servers and its data providers
	if recq.IntraMessage == false {
		recq.IntraMessage = true
		// to other computing servers
		err := libunlynx.SendISMOthers(s.ServiceProcessor, &recq.RosterServers, recq)
		if err != nil {
			log.Error("[SERVICE] <LEMAL> Server, broadcasting [SurveyQuery] error ", err)
		}
		recq.IntraMessage = false
	}

	// to the DPs
	listDPs := generateDataCollectionRoster(s.ServerIdentity(), recq.ServerToDP)
	if listDPs != nil {
		err := libunlynx.SendISMOthers(s.ServiceProcessor, listDPs, &lib.SurveyQueryToDP{SQ: *recq, Root: s.ServerIdentity()})
		if err != nil {
			log.Error("[SERVICE] <LEMAL> Server, broadcasting [SurveyQuery] error ", err)
		}
	}

	// DRO Phase
	if recq.IntraMessage == false {
		go func() {
			//diffPTimer := libunlynx.StartTimer(s.ServerIdentity().String() + "_DiffPPhase")
			if lib.AddDiffP(castToSurvey(s.Survey.Get(recq.SurveyID)).SurveyQuery.Query.DiffP) {
				s.DROPhase(castToSurvey(s.Survey.Get(recq.SurveyID)).SurveyQuery.SurveyID)
			}
			//libunlynx.EndTimer(diffPTimer)
		}()
	}

	// wait for all DPs to get the query
	if listDPs != nil {
		counter := len(*recq.ServerToDP[s.ServerIdentity().String()])
		for counter > 0 {
			counter = counter - (<-castToSurvey(s.Survey.Get(recq.SurveyID)).DPqueryChannel)
		}
	}

	// TODO: we can remove this waiting after the test
	// -----------------------------------------------------------------------------------------------------------------
	// signal other nodes that the data provider(s) already sent their data (response)
	err := libunlynx.SendISMOthers(s.ServiceProcessor, &recq.RosterServers, &SyncDCP{recq.SurveyID})
	if err != nil {
		log.Error("[SERVICE] <LEMAL> Server, broadcasting [syncDCPChannel] error ", err)
	}

	counter := len(recq.RosterServers.List) - 1
	for counter > 0 {
		counter = counter - (<-castToSurvey(s.Survey.Get(recq.SurveyID)).SyncDCPChannel)
	}
	// -----------------------------------------------------------------------------------------------------------------

	startDataCollectionProtocol := libunlynx.StartTimer(s.ServerIdentity().String() + "_DataCollectionProtocol")
	if listDPs != nil {
		// servers contact their DPs to get their response
		if err := s.DataCollectionPhase(recq.SurveyID); err != nil {
			log.Error("[SERVICE] <LEMAL> Server, data collection error", err)
		}
		libunlynx.EndTimer(startDataCollectionProtocol)

	}

	//startWaitTimeDPs := libunlynx.StartTimer(s.ServerIdentity().String() + "_WaitTimeDPs")
	// signal other nodes that the data provider(s) already sent their data (response)
	err = libunlynx.SendISMOthers(s.ServiceProcessor, &recq.RosterServers, &DPdataFinished{recq.SurveyID})
	if err != nil {
		log.Error("[SERVICE] <LEMAL> Server, broadcasting [DPdataFinished] error ", err)
	}

	counter = len(recq.RosterServers.List) - 1
	for counter > 0 {
		log.Lvl2("[SERVICE] <LEMAL> Server", s.ServerIdentity(), "is waiting for", counter, "servers to finish collecting their data")
		counter = counter - (<-castToSurvey(s.Survey.Get(recq.SurveyID)).DPdataChannel)
	}
	log.Lvl2("[SERVICE] <LEMAL> Server", s.ServerIdentity(), "- all data providers have sent their data")

	//libunlynx.EndTimer(startWaitTimeDPs)

	// ready to start the collective aggregation & key switching protocol
	if recq.IntraMessage == false {
		startJustExecution := libunlynx.StartTimer("JustExecution")
		s.StartService(recq.SurveyID)

		log.Lvl2("[SERVICE] <LEMAL> Server", s.ServerIdentity(), " completed the query processing...")

		survey := castToSurvey(s.Survey.Get(recq.SurveyID))
		result := survey.QueryResponseState
		libunlynx.EndTimer(startJustExecution)
		return &result, nil
	}

	return nil, nil
}

// Protocol Handlers
//______________________________________________________________________________________________________________________

// NewProtocol creates a protocol instance executed by all nodes
func (s *ServiceLeMal) NewProtocol(tn *onet.TreeNodeInstance, conf *onet.GenericConfig) (onet.ProtocolInstance, error) {
	tn.SetConfig(conf)

	var pi onet.ProtocolInstance
	var err error

	target := string(conf.Data)

	switch tn.ProtocolName() {
	case protocols.ProofCollectionProtocolName:
		return s.NewProofCollectionProtocolInstance(tn, target)
	case protocols.DataCollectionProtocolName:
		pi, err = protocols.NewDataCollectionProtocol(tn)
		if err != nil {
			return nil, err
		}

		if !tn.IsRoot() {
			survey := castToSurvey(s.Survey.Get(target))
			dataCollectionProtocol := pi.(*protocols.DataCollectionProtocol)

			queryStatement := protocols.SurveyToDP{
				SurveyID:  survey.SurveyQuery.SurveyID,
				Aggregate: survey.SurveyQuery.RosterServers.Aggregate,
				Query:     survey.SurveyQuery.Query,
			}
			dataCollectionProtocol.Survey = queryStatement
			dataCollectionProtocol.MapPIs = survey.MapPIs
		}
		return pi, nil

	case protocols.CollectiveAggregationProtocolName:
		survey := castToSurvey(s.Survey.Get(target))
		pi, err = protocols.NewCollectiveAggregationProtocol(tn)
		if err != nil {
			return nil, err
		}

		// convert the result to fit the collective aggregation protocol
		groupedData := libunlynx.ConvertToAggregationStruct(survey.QueryResponseState)

		if survey.SurveyQuery.Query.Proofs != 0 {
			go func() {
				log.Lvl2("SERVICE] <LEMAL> Server", s.ServerIdentity(), "creates local aggregation proof")
				resultAggrLocal := libunlynx.ResponseAllDPs{}
				for i, v := range groupedData {
					resultAggrLocal.Data = append(resultAggrLocal.Data, libunlynx.ResponseDPOneGroup{Group: string(i), Data: v.AggregatingAttributes})
				}
				aggrLocalProof := proof.ServerAggregationProofCreation(survey.QueryResponseState, resultAggrLocal)
				if survey.SurveyQuery.Query.Proofs == 2 {
					aggrLocalProof.DPsData = libunlynx.ResponseAllDPs{}
				}

				pi := survey.MapPIs["aggregation/"+s.ServerIdentity().String()]
				pi.(*protocols.ProofCollectionProtocol).Proof = lib.ProofRequest{AggregationProof: lib.NewAggregationProofRequest(&aggrLocalProof, target, s.ServerIdentity().String(), "", survey.SurveyQuery.Query.RosterVNs, tn.Private(), nil)}
				go pi.Dispatch()
				go pi.Start()
				<-pi.(*protocols.ProofCollectionProtocol).FeedbackChannel
			}()
		}

		collectiveAggregation := pi.(*protocols.CollectiveAggregationProtocol)
		collectiveAggregation.GroupedData = &groupedData

		return pi, nil
	case protocols.ObfuscationProtocolName:
		survey := castToSurvey(s.Survey.Get(target))
		pi, err = protocols.NewObfuscationProtocol(tn)
		if err != nil {
			return nil, err
		}

		obfuscation := pi.(*protocols.ObfuscationProtocol)

		obfuscation.ToObfuscateData = *convertToCipherVector(&survey.QueryResponseState)
		obfuscation.Proofs = survey.SurveyQuery.Query.Proofs
		obfuscation.Query = &survey.SurveyQuery
		obfuscation.MapPIs = survey.MapPIs

	case protocolsunlynx.DROProtocolName:
		survey := castToSurvey(s.Survey.Get(target))
		log.Lvl2("SERVICE] <LEMAL> Server", s.ServerIdentity(), " Servers collectively add noise for differential privacy")
		pi, err := protocols.NewShufflingProtocol(tn)
		if err != nil {
			return nil, err
		}

		shuffle := pi.(*protocols.ShufflingProtocol)
		shuffle.Proofs = survey.SurveyQuery.Query.Proofs
		shuffle.Precomputed = survey.ShufflePrecompute
		shuffle.Query = &survey.SurveyQuery
		shuffle.MapPIs = survey.MapPIs

		if tn.IsRoot() {
			clientResponses := make([]libunlynx.ProcessResponse, 0)
			if survey.SurveyQuery.Query.DiffP.Scale == 0 {
				survey.SurveyQuery.Query.DiffP.Scale = 1
			}
			noiseArray := libunlynx.GenerateNoiseValuesScale(int64(survey.SurveyQuery.Query.DiffP.NoiseListSize), survey.SurveyQuery.Query.DiffP.LapMean, survey.SurveyQuery.Query.DiffP.LapScale, survey.SurveyQuery.Query.DiffP.Quanta, survey.SurveyQuery.Query.DiffP.Scale, survey.SurveyQuery.Query.DiffP.Limit)
			for _, v := range noiseArray {
				clientResponses = append(clientResponses, libunlynx.ProcessResponse{GroupByEnc: nil, AggregatingAttributes: libunlynx.IntArrayToCipherVector([]int64{int64(v)})})
			}
			shuffle.TargetOfShuffle = &clientResponses
		}
		return pi, nil

	case protocols.KeySwitchingProtocolName:
		survey := castToSurvey(s.Survey.Get(target))
		pi, err = protocols.NewKeySwitchingProtocol(tn)
		if err != nil {
			return nil, err
		}
		keySwitch := pi.(*protocols.KeySwitchingProtocol)
		keySwitch.Proofs = survey.SurveyQuery.Query.Proofs
		keySwitch.Query = &survey.SurveyQuery
		keySwitch.MapPIs = survey.MapPIs

		if tn.IsRoot() {
			if lib.AddDiffP(survey.SurveyQuery.Query.DiffP) {
				for i, v := range survey.QueryResponseState.Data {
					survey.QueryResponseState.Data[i].Data.Add(v.Data, survey.Noises[:len(v.Data)])
				}
			}
			keySwitch.TargetOfSwitch = convertToCipherVector(&survey.QueryResponseState)
			tmp := survey.SurveyQuery.ClientPubKey
			keySwitch.TargetPublicKey = &tmp

			s.Survey.Put(string(target), survey)

		}
		return pi, nil

	default:
		return nil, errors.New("Service attempts to start an unknown protocol: " + tn.ProtocolName() + ".")
	}

	return pi, nil
}

// StartProtocol starts a specific protocol
func (s *ServiceLeMal) StartProtocol(name string, targetSurvey string) (onet.ProtocolInstance, error) {
	// this generates the PIs of proof collection to be run inside the protocols
	tmp := castToSurvey(s.Survey.Get((string)(targetSurvey)))

	var tree *onet.Tree
	if name == protocols.DataCollectionProtocolName {
		tree = generateDataCollectionRoster(s.ServerIdentity(), tmp.SurveyQuery.ServerToDP).GenerateStar()
	} else {
		tree = tmp.SurveyQuery.RosterServers.GenerateNaryTreeWithRoot(2, s.ServerIdentity())
	}

	var tn *onet.TreeNodeInstance
	tn = s.NewTreeNodeInstance(tree, tree.Root, name)

	conf := onet.GenericConfig{Data: []byte(string(targetSurvey))}

	pi, err := s.NewProtocol(tn, &conf)
	if err != nil {
		log.Fatal("Error running" + name)
	}

	s.RegisterProtocolInstance(pi)
	go pi.Dispatch()
	go pi.Start()

	return pi, err
}

// Service Phases
//______________________________________________________________________________________________________________________

// StartService starts the service (with all its different steps/protocols)
func (s *ServiceLeMal) StartService(targetSurvey string) error {
	log.Lvl2("SERVICE] <LEMAL> Server", s.ServerIdentity(), " starts a collective aggregation, (differential privacy) & key switching for survey ", targetSurvey)

	target := castToSurvey(s.Survey.Get((string)(targetSurvey)))

	// Aggregation Phase
	//aggregationTimer := libunlynx.StartTimer(s.ServerIdentity().String() + "_AggregationPhase")
	err := s.AggregationPhase(target.SurveyQuery.SurveyID)
	if err != nil {
		log.Fatal("Error in the Aggregation Phase")
	}
	//libunlynx.EndTimer(aggregationTimer)

	if target.SurveyQuery.Query.Obfuscation {
		//obfuscationTimer := libunlynx.StartTimer(s.ServerIdentity().String() + "_ObfuscationPhase")
		err := s.ObfuscationPhase(target.SurveyQuery.SurveyID)
		if err != nil {
			log.Fatal("Error in the Obfuscation Phase")
		}
		//libunlynx.EndTimer(obfuscationTimer)
	}

	// DRO Phase
	if lib.AddDiffP(target.SurveyQuery.Query.DiffP) {
		<-target.DiffPChannel
	}

	// Key Switch Phase
	//keySwitchTimer := libunlynx.StartTimer(s.ServerIdentity().String() + "_KeySwitchingPhase")
	err = s.KeySwitchingPhase(target.SurveyQuery.SurveyID)
	if err != nil {
		log.Fatal("Error in the Key Switching Phase")
	}
	//libunlynx.EndTimer(keySwitchTimer)

	return nil
}

func (s *ServiceLeMal) DataCollectionPhase(targetSurvey string) error {
	pi, err := s.StartProtocol(protocols.DataCollectionProtocolName, targetSurvey)
	if err != nil {
		return err
	}
	dataDPs := <-pi.(*protocols.DataCollectionProtocol).FeedbackChannel

	survey := castToSurvey(s.Survey.Get((string)(targetSurvey)))
	// we convert the map into an object of [Group + CipherVector] to avoid later problems with protobuf
	for key, value := range dataDPs {
		if survey.SurveyQuery.Query.CuttingFactor != 0 {
			survey.QueryResponseState.Data = append(survey.QueryResponseState.Data, libunlynx.ResponseDPOneGroup{Group: key, Data: value[:int(len(value)/survey.SurveyQuery.Query.CuttingFactor)]})
		} else {
			survey.QueryResponseState.Data = append(survey.QueryResponseState.Data, libunlynx.ResponseDPOneGroup{Group: key, Data: value})

		}
	}
	s.Survey.Put(string(targetSurvey), survey)
	return nil
}

// AggregationPhase performs the per-group aggregation on the currently grouped data.
func (s *ServiceLeMal) AggregationPhase(targetSurvey string) error {
	pi, err := s.StartProtocol(protocols.CollectiveAggregationProtocolName, targetSurvey)
	if err != nil {
		return err
	}
	cothorityAggregatedData := <-pi.(*protocols.CollectiveAggregationProtocol).FeedbackChannel

	survey := castToSurvey(s.Survey.Get((string)(targetSurvey)))

	survey.QueryResponseState = *libunlynx.ConvertFromAggregationStruct(cothorityAggregatedData)
	s.Survey.Put(string(targetSurvey), survey)
	return nil
}

// ObfuscationPhase performs the obfuscation phase (multiply the aggregated data by a random value from each server)
func (s *ServiceLeMal) ObfuscationPhase(targetSurvey string) error {
	pi, err := s.StartProtocol(protocols.ObfuscationProtocolName, targetSurvey)
	if err != nil {
		return err
	}
	obfuscationData := <-pi.(*protocols.ObfuscationProtocol).FeedbackChannel

	survey := castToSurvey(s.Survey.Get((string)(targetSurvey)))
	survey.QueryResponseState = *convertFromKeySwitchingStruct(obfuscationData, survey.QueryResponseState)
	s.Survey.Put(string(targetSurvey), survey)
	return nil
}

// DROPhase shuffles the list of noise values.
func (s *ServiceLeMal) DROPhase(targetSurvey string) error {
	pi, err := s.StartProtocol(protocolsunlynx.DROProtocolName, targetSurvey)
	if err != nil {
		return err
	}

	shufflingResult := <-pi.(*protocols.ShufflingProtocol).FeedbackChannel

	survey := castToSurvey(s.Survey.Get((string)(targetSurvey)))
	noises := *libunlynx.NewCipherVector(len(shufflingResult))
	for i, v := range shufflingResult {
		noises[i] = v.AggregatingAttributes[0]
	}
	survey.Noises = noises
	survey.DiffPChannel <- 1
	s.Survey.Put(string(targetSurvey), survey)
	return nil
}

// KeySwitchingPhase performs the switch to the querier's key on the currently aggregated data.
func (s *ServiceLeMal) KeySwitchingPhase(targetSurvey string) error {
	pi, err := s.StartProtocol(protocols.KeySwitchingProtocolName, targetSurvey)
	if err != nil {
		return err
	}
	keySwitchedAggregatedResponses := <-pi.(*protocols.KeySwitchingProtocol).FeedbackChannel

	survey := castToSurvey(s.Survey.Get((string)(targetSurvey)))
	survey.QueryResponseState = *convertFromKeySwitchingStruct(keySwitchedAggregatedResponses, survey.QueryResponseState)
	s.Survey.Put(targetSurvey, survey)
	return err
}

// Support Functions
//______________________________________________________________________________________________________________________

// these first four functions are used to adapat the existing protocols to the 'lemal' service structs
func convertToCipherVector(ad *libunlynx.ResponseAllDPs) *libunlynx.CipherVector {
	cv := make(libunlynx.CipherVector, 0)
	for _, response := range ad.Data {
		cv = append(cv, response.Data...)
	}
	return &cv
}

func convertFromKeySwitchingStruct(cv libunlynx.CipherVector, dpResponses libunlynx.ResponseAllDPs) *libunlynx.ResponseAllDPs {
	data := make([]libunlynx.ResponseDPOneGroup, 0)

	length := len(dpResponses.Data[0].Data)
	init := 0
	groupIndex := 0
	for i := 1; i <= len(cv); i++ {
		if i%length == 0 {
			tmp := cv[init:i]
			init = i
			data = append(data, libunlynx.ResponseDPOneGroup{Group: dpResponses.Data[groupIndex].Group, Data: tmp})
			groupIndex++
		}
	}
	return &libunlynx.ResponseAllDPs{Data: data}

}

func generateDataCollectionRoster(root *network.ServerIdentity, serverToDP map[string]*[]network.ServerIdentity) *onet.Roster {
	for key, value := range serverToDP {
		if key == root.String() {
			roster := make([]*network.ServerIdentity, 0)
			roster = append(roster, root)

			for _, srv := range *value {
				tmp := srv
				roster = append(roster, &tmp)
			}
			return onet.NewRoster(roster)
		}
	}

	return nil
}

func recreateRangeSignatures(ivSigs lib.QueryIVSigs) []*[]proof.PublishSignatureBytes {
	recreate := make([]*[]proof.PublishSignatureBytes, 0)

	// transform the one-dimensional array (because of protobuf) to the original two-dimensional array
	indexInit := 0
	for i := 1; i <= len(ivSigs.InputValidationSigs); i++ {
		if i%ivSigs.InputValidationSize2 == 0 {
			tmp := make([]proof.PublishSignatureBytes, ivSigs.InputValidationSize2)
			for j := range tmp {
				tmp[j] = (*ivSigs.InputValidationSigs[indexInit])[0]
				indexInit++
			}
			recreate = append(recreate, &tmp)

			indexInit = i
		}

	}
	return recreate
}
