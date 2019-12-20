package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/rpc"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/proto"
	mspclient "github.com/hyperledger/fabric-sdk-go/pkg/client/msp"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/resmgmt"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/context"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/msp"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config/lookup"
	"github.com/hyperledger/fabric-sdk-go/pkg/fabsdk"
)

type Mod string
type ConsensusState string

const (
	ModifiedModAdd Mod = "Add"
	ModifiedModDel Mod = "Del"

	StateNormal      ConsensusState = "STATE_NORMAL"
	StateMaintenance ConsensusState = "STATE_MAINTENANCE"
)

var (
	client *rpc.Client
)

func GetConsensusState(status string) ConsensusState {
	switch status {
	case "Normal":
		return StateNormal
	case "Maintenance":
		return StateMaintenance
	}
	return ""
}

func SDKNew(fabconfig string) *fabsdk.FabricSDK {
	sdk, err := fabsdk.New(config.FromFile(fabconfig))
	if err != nil {
		log.Fatalln("new fabsdk error:", err)
	}
	return sdk
}

func GetOrgsTargetPeers(sdk *fabsdk.FabricSDK, orgsName []string) ([]string, error) {
	configBackend, err := sdk.Config()
	if err != nil {
		return nil, errors.New(fmt.Sprintf("get orgs target peers error: %s", err))
	}

	networkConfig := fab.NetworkConfig{}
	if err := lookup.New(configBackend).UnmarshalKey("organizations", &networkConfig.Organizations); err != nil {
		return nil, errors.New(fmt.Sprintf("lookup unmarshal key error: %s", err))
	}

	var peers []string
	for _, org := range orgsName {
		orgConfig, ok := networkConfig.Organizations[strings.ToLower(org)]
		if !ok {
			continue
		}
		peers = append(peers, orgConfig.Peers...)
	}

	return peers, nil
}

func GetSigningIdentities(ctx context.ClientProvider, orgs []string) []msp.SigningIdentity {
	signingIdentities := make([]msp.SigningIdentity, 0)
	for _, orgName := range orgs {
		mspClient, err := mspclient.New(ctx, mspclient.WithOrg(orgName))
		if err != nil {
			log.Fatalf("%s msp new error: %s", orgName, err)
		}
		identity, err := mspClient.GetSigningIdentity("Admin")
		if err != nil {
			log.Fatalf("%s get signing identity error: %s", orgName, err)
		}

		signingIdentities = append(signingIdentities, identity)
	}

	return signingIdentities
}

func InitRPCClient(address string) {
	var err error

	if client == nil {
		client, err = rpc.DialHTTP("tcp", address)
		if err != nil {
			log.Fatalln("dialling rpc error:", err)
		}
	}
}

func protoDecode(msgName string, input []byte) ([]byte, error) {
	return protoEncodeAndDecode("Proto.Decode", msgName, input)
}

func protoEncode(msgName string, input []byte) ([]byte, error) {
	return protoEncodeAndDecode("Proto.Encode", msgName, input)
}

func protoEncodeAndDecode(typ, msgName string, input []byte) ([]byte, error) {
	var reply []byte

	if err := client.Call(typ, struct {
		MsgName string
		Input   []byte
	}{
		msgName,
		input,
	}, &reply); err != nil {
		return nil, err
	}

	return reply, nil
}

func computeUpdate(channelName string, origin, updated []byte) ([]byte, error) {
	var reply []byte

	if err := client.Call("Compute.Update", struct {
		ChannelName string
		Origin      []byte
		Updated     []byte
	}{
		channelName,
		origin,
		updated,
	}, &reply); err != nil {
		return nil, err
	}

	return reply, nil
}

func GetStdConfigBytes(mspID string, configBytes []byte) []byte {
	format := `{"channel_group":{"groups":{"Application":{"groups":{"%s":%s}}}}}`
	return []byte(fmt.Sprintf(format, mspID, string(configBytes)))
}

func GetStdUpdateEnvelopBytes(channelName string, updateEnvelopBytes []byte) []byte {
	format := `{"payload":{"header":{"channel_header":{"channel_id":"%s", "type":2}},"data":{"config_update":%s}}}`
	return []byte(fmt.Sprintf(format, channelName, string(updateEnvelopBytes)))
}

func GetNewestConfigWithConfigBlock(resMgmt *resmgmt.Client, channelName string, sysChannel bool) []byte {
	blockPB, err := resMgmt.QueryConfigBlockFromOrderer(channelName)
	if err != nil {
		log.Fatalln(err)
	}
	blockPBBytes, err := proto.Marshal(blockPB)
	if err != nil {
		log.Fatalln(err)
	}

	blockBytes, err := protoDecode("common.Block", blockPBBytes)
	if err != nil {
		log.Fatalln("proto decode common.Block error:", err)
	}

	var block interface{}
	if sysChannel {
		block = new(SystemBlock)
	} else {
		block = new(Block)
	}
	if err := json.Unmarshal(blockBytes, block); err != nil {
		log.Fatalln("unmarshal block json error:", err)
	}

	var cfg interface{}
	if sysChannel {
		cfg = block.(*SystemBlock).Data.Data[0].Payload.Data.Config
	} else {
		cfg = block.(*Block).Data.Data[0].Payload.Data.Config
	}

	configBytes, err := json.Marshal(cfg)
	if err != nil {
		log.Fatalln("marshal config json error:", err)
	}

	return configBytes
}

func GetNewOrgConfigWithFielePath(filePath, mspID string) []byte {
	newOrgFileBytes, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Fatalln(err)
	}

	return GetStdConfigBytes(mspID, newOrgFileBytes)
}

func GetModifiedConfig(configBytes []byte, newOrgConfigBytes []byte, mod Mod, ordererOrg, sysChannel bool) []byte {
	var cfg interface{}

	if configBytes != nil {
		if sysChannel {
			cfg = new(SystemConfig)
		} else {
			cfg = new(Config)
		}

		if err := json.Unmarshal(configBytes, cfg); err != nil {
			log.Fatalln(err)
		}
	}

	newOrgConfig := new(Config)
	orgName := ""
	switch mod {
	case ModifiedModAdd:
		if newOrgConfigBytes != nil {
			if err := json.Unmarshal(newOrgConfigBytes, newOrgConfig); err != nil {
				log.Fatalln(err)
			}
		}
	case ModifiedModDel:
		orgName = string(newOrgConfigBytes)
	}

	switch mod {
	case ModifiedModAdd:
		if sysChannel {
			if ordererOrg {
				for orgName, org := range newOrgConfig.ChannelGroup.Groups.Application.Groups {
					cfg.(*SystemConfig).ChannelGroup.Groups.Orderer.Groups[orgName] = org
				}
				break
			}
			for orgName, org := range newOrgConfig.ChannelGroup.Groups.Application.Groups {
				cfg.(*SystemConfig).ChannelGroup.Groups.Consortiums.Groups.SampleConsortium.Groups[orgName] = org
			}
		} else {
			if ordererOrg {
				for orgName, org := range newOrgConfig.ChannelGroup.Groups.Application.Groups {
					cfg.(*Config).ChannelGroup.Groups.Orderer.Groups[orgName] = org
				}
				break
			}
			for orgName, org := range newOrgConfig.ChannelGroup.Groups.Application.Groups {
				cfg.(*Config).ChannelGroup.Groups.Application.Groups[orgName] = org
			}
		}
	case ModifiedModDel:
		if sysChannel {
			if ordererOrg {
				delete(cfg.(*SystemConfig).ChannelGroup.Groups.Orderer.Groups, orgName)
				break
			}
			delete(cfg.(*SystemConfig).ChannelGroup.Groups.Consortiums.Groups.SampleConsortium.Groups, orgName)
		} else {
			if ordererOrg {
				delete(cfg.(*Config).ChannelGroup.Groups.Orderer.Groups, orgName)
			}
			delete(cfg.(*Config).ChannelGroup.Groups.Application.Groups, orgName)
		}
	}

	modifiedConfigBytes, err := json.Marshal(cfg)
	if err != nil {
		log.Fatalln("marshal modified cfg json error:", err)
	}

	return modifiedConfigBytes
}

func GetChannelParamsModifiedConfig(configBytes []byte,
	batchTimeout, batchSizeAbsolute, batchSizePreferred string, batchSizeMessage int, sysChannel bool) []byte {

	var cfg interface{}

	if configBytes != nil {
		if sysChannel {
			cfg = new(SystemConfig)
		} else {
			cfg = new(Config)
		}

		if err := json.Unmarshal(configBytes, cfg); err != nil {
			log.Fatalln(err)
		}
	}

	var values map[string]interface{}
	if sysChannel {
		values = cfg.(*SystemConfig).ChannelGroup.Groups.Orderer.Values
	} else {
		values = cfg.(*Config).ChannelGroup.Groups.Orderer.Values
	}

	if batchTimeout != "" {
		values["BatchTimeout"].(map[string]interface{})["value"].(map[string]interface{})["timeout"] = batchTimeout
	}

	if batchSizeAbsolute != "" {
		values["BatchSize"].(map[string]interface{})["value"].(map[string]interface{})["absolute_max_bytes"] = convertStorageUnit(batchSizeAbsolute)
	}

	if batchSizeMessage != 0 {
		values["BatchSize"].(map[string]interface{})["value"].(map[string]interface{})["max_message_count"] = batchSizeMessage
	}

	if batchSizePreferred != "" {
		values["BatchSize"].(map[string]interface{})["value"].(map[string]interface{})["preferred_max_bytes"] = convertStorageUnit(batchSizePreferred)
	}

	modifiedConfigBytes, err := json.Marshal(cfg)
	if err != nil {
		log.Fatalln("marshal modified cfg json error:", err)
	}

	return modifiedConfigBytes
}

func GetChannelConsensusStateModifiedConfig(configBytes []byte, consensusState ConsensusState, consensusType string,
	consensusOptionElectionTick, consensusOptionHeartbeatTick, consensusOptionMaxInflightBlocks int,
	consensusOptionSnapshotIntervalSize, consensusOptionTickInterval string,
	sysChannel bool) []byte {
	var cfg interface{}

	if configBytes != nil {
		if sysChannel {
			cfg = new(SystemConfig)
		} else {
			cfg = new(Config)
		}

		if err := json.Unmarshal(configBytes, cfg); err != nil {
			log.Fatalln(err)
		}
	}

	var values map[string]interface{}
	if sysChannel {
		values = cfg.(*SystemConfig).ChannelGroup.Groups.Orderer.Values
	} else {
		values = cfg.(*Config).ChannelGroup.Groups.Orderer.Values
	}

	consensusTypeMap := getMap(values, "ConsensusType")
	valueMap := getMap(consensusTypeMap, "value")
	metadataMap := getMap(valueMap, "metadata")
	optionsMap := getMap(metadataMap, "options")

	switch {
	case consensusState != "":
		valueMap["state"] = consensusState
	case consensusType != "":
		if consensusType == "etcdraft" {
			if consensusOptionElectionTick == 0 {
				consensusOptionElectionTick = 10
			}
			if consensusOptionHeartbeatTick == 0 {
				consensusOptionHeartbeatTick = 1
			}
			if consensusOptionMaxInflightBlocks == 0 {
				consensusOptionMaxInflightBlocks = 5
			}
			if consensusOptionSnapshotIntervalSize == "" {
				consensusOptionSnapshotIntervalSize = "20MB"
			}
			if consensusOptionTickInterval == "" {
				consensusOptionTickInterval = "500ms"
			}
		}
		valueMap["type"] = consensusType
		fallthrough
	case consensusOptionElectionTick != 0:
		optionsMap["election_tick"] = consensusOptionElectionTick
		fallthrough
	case consensusOptionHeartbeatTick != 0:
		optionsMap["heartbeat_tick"] = consensusOptionElectionTick
		fallthrough
	case consensusOptionMaxInflightBlocks != 0:
		optionsMap["max_inflight_blocks"] = consensusOptionMaxInflightBlocks
		fallthrough
	case consensusOptionSnapshotIntervalSize != "":
		optionsMap["snapshot_interval_size"] = convertStorageUnit(consensusOptionSnapshotIntervalSize)
		fallthrough
	case consensusOptionTickInterval != "":
		optionsMap["tick_interval"] = consensusOptionTickInterval
	}

	modifiedConfigBytes, err := json.Marshal(cfg)
	if err != nil {
		log.Fatalln("marshal modified cfg json error:", err)
	}

	return modifiedConfigBytes
}

func GetUpdateEnvelopeProtoBytes(configBytes, modifiedConfigBytes []byte, channelName string) []byte {
	configPBBytes, err := protoEncode("common.Config", configBytes)
	if err != nil {
		log.Fatalln("proto encode common.Config error:", err)
	}

	// get modified config.pb
	modifiedConfigPBBytes, err := protoEncode("common.Config", modifiedConfigBytes)
	if err != nil {
		log.Fatalln("proto encode common.Config error:", err)
	}

	// get update.pb
	updateConfigPBBytes, err := computeUpdate(channelName, configPBBytes, modifiedConfigPBBytes)
	if err != nil {
		log.Fatalln("compute update error:", err)
	}

	// get update.json
	updateConfigBytes, err := protoDecode("common.ConfigUpdate", updateConfigPBBytes)
	if err != nil {
		log.Fatalln("proto decode common.ConfigUpdate error:", err)
	}
	updateEnvelopeBytes := GetStdUpdateEnvelopBytes(channelName, updateConfigBytes)

	// get update.pb
	updateEnvelopePBBytes, err := protoEncode("common.Envelope", updateEnvelopeBytes)
	if err != nil {
		log.Fatalln("proto encode common.Envelope error:", err)
	}

	return updateEnvelopePBBytes
}

func convertStorageUnit(data string) int64 {
	var KB int64 = 1024
	var MB = 1024 * KB

	num, err := strconv.Atoi(data[:len(data)-2])
	if err != nil {
		log.Fatalln("strconv atoi error:", err)
	}

	data = strings.ToLower(data)
	if strings.Contains(data, "kb") {
		return int64(num) * KB
	}

	if strings.Contains(data, "mb") {
		return int64(num) * MB
	}

	return 0
}

func getMap(data map[string]interface{}, key string) map[string]interface{} {
	if data[key] == nil {
		data[key] = make(map[string]interface{})
	}
	return data[key].(map[string]interface{})
}
