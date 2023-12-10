package shardkv

import (
	"bytes"
	"cs651/labrpc"
	"cs651/shardmaster"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"cs651/labgob"
	"cs651/raft"
)

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

type KvOp int32

const (
	KvOp_Get               KvOp = 0
	KvOp_Put               KvOp = 1
	KvOp_Append            KvOp = 2
	KvOp_Config            KvOp = 3
	KvOp_Migration         KvOp = 4
	KvOp_GarbageCollection KvOp = 5
)

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	OpType         KvOp
	Key            string
	Value          string
	Id             int64
	SeqNum         int64
	Err            Err
	ConfigNumber   int
	MigrationReply GetMigrationReply
	Config         shardmaster.Config
	GCNum          int
	GCShard        int
}

type ShardKV struct {
	mu           sync.Mutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	make_end     func(string) *labrpc.ClientEnd
	gid          int
	masters      []*labrpc.ClientEnd
	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	dead        int32
	lastApplied int
	db          [shardmaster.NShards]map[string]string
	// Key: index Value: op
	channels map[int]chan Op
	// clients sequence number
	clients   [shardmaster.NShards]map[int64]int64
	configs   []shardmaster.Config
	oldConfig shardmaster.Config
	pdclient  *shardmaster.Clerk

	availableShards map[int]bool
	oldshards       map[int]map[int]bool

	requiredShards map[int]bool
	// config id -> shard id -> data
	oldshardsData map[int]map[int]map[string]string
	// config id -> shard id -> seq
	oldshardsSeq map[int]map[int]map[int64]int64
	// config id -> shard id
	garbageList map[int]map[int]bool
}

// use it with lock, be careful :)
func (kv *ShardKV) latestConfig() shardmaster.Config {
	return kv.configs[len(kv.configs)-1]
}

func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	kv.mu.Lock()

	hashVal := key2shard(args.Key)
	_, isLeader := kv.rf.GetState()
	_, shardOk := kv.availableShards[hashVal]
	if isLeader && (args.CfgNum != kv.latestConfig().Num || !shardOk) {
		reply.Err = ErrWrongGroup
		kv.mu.Unlock()
		return
	}

	seq, ok := kv.clients[hashVal][args.Id]
	if isLeader && ok && seq >= args.SeqNum {
		DPrintf("Server %v replies client Get(%v) seq : %v arg seq: %v client id: %v",
			kv.me, args.Key, seq, args.SeqNum, args.Id)
		value, exists := kv.db[hashVal][args.Key]
		if exists {
			reply.Err = OK
			reply.Value = value
		} else {
			reply.Err = ErrNoKey
		}
		kv.mu.Unlock()
		return
	}
	// Your code here.
	op := Op{
		OpType:       KvOp_Get,
		Key:          args.Key,
		Value:        "",
		Id:           args.Id,
		SeqNum:       args.SeqNum,
		ConfigNumber: kv.latestConfig().Num}
	kv.mu.Unlock()
	index, _, isLeader := kv.rf.Start(op)

	// otherwise, push the log
	if isLeader {
		DPrintf("Server %v send Start Request in Get, Index: %v",
			kv.me, index)

		kv.mu.Lock()
		resChan := make(chan Op, 1)
		kv.channels[index] = resChan
		kv.mu.Unlock()
		select {
		case op := <-resChan:
			{
				reply.Err = op.Err
				reply.Value = op.Value
				DPrintf("Server %v replies client Get(%v): %v ", kv.me, args.Key, reply.Err)
			}
		case <-time.After(time.Millisecond * 800):
			{
				reply.Err = ErrWrongLeader
				DPrintf("Server %v timeout replies client Get(%v): %v ", kv.me, args.Key, reply.Err)
			}
		}

		kv.mu.Lock()
		delete(kv.channels, index)
		kv.mu.Unlock()

	} else {
		DPrintf("Server %v is not the leader now", kv.me)
		reply.Err = ErrWrongLeader
	}
}

func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.

	kv.mu.Lock()

	hashVal := key2shard(args.Key)
	_, isLeader := kv.rf.GetState()

	_, shardOk := kv.availableShards[hashVal]
	if isLeader && (args.CfgNum != kv.latestConfig().Num || !shardOk) {
		reply.Err = ErrWrongGroup
		kv.mu.Unlock()
		return
	}

	seq, ok := kv.clients[hashVal][args.Id]
	if isLeader && ok && seq >= args.SeqNum {
		reply.Err = OK
		DPrintf("Server replies client PutAppend(%v, %v): %v seq: %v arg seq: %v client id : %v",
			args.Key, args.Value, reply.Err, args.SeqNum, seq, args.Id)

		kv.mu.Unlock()
		return
	}

	opType := KvOp_Put

	if args.Op == "Append" {
		opType = KvOp_Append
	}

	op := Op{
		OpType:       opType,
		Key:          args.Key,
		Value:        args.Value,
		Id:           args.Id,
		SeqNum:       args.SeqNum,
		ConfigNumber: kv.latestConfig().Num}

	kv.mu.Unlock()
	index, _, isLeader := kv.rf.Start(op)
	DPrintf("Server %v send Start Request in PutAppend at index %v", kv.me, index)
	if isLeader {
		kv.mu.Lock()
		resChan := make(chan Op, 1)
		kv.channels[index] = resChan
		kv.mu.Unlock()
		DPrintf("Server %v waits for result in PutAppend at index %v", kv.me, index)
		select {
		case <-resChan:
			{
				reply.Err = op.Err
				DPrintf("Server %v replies client PutAppend(%v, %v): %v",
					kv.me, args.Key, args.Value, reply.Err)
			}
		case <-time.After(time.Millisecond * 800):
			{
				reply.Err = ErrWrongLeader
				DPrintf("Server %v (timeout) replies client PutAppend(%v, %v): %v",
					kv.me, args.Key, args.Value, reply.Err)
			}

		}

		kv.mu.Lock()
		delete(kv.channels, index)
		kv.mu.Unlock()

	} else {
		reply.Err = ErrWrongLeader
		DPrintf("Server %v is not leader now", kv.me)
	}
}

func (kv *ShardKV) GetMigration(args *GetMigrationArgs, reply *GetMigrationReply) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	if oldShards, versionOk := kv.oldshards[args.Num]; versionOk {
		if _, shardOk := oldShards[args.Shard]; shardOk {

			// config number -> shard number
			reply.Data = make(map[string]string)
			for k, v := range kv.oldshardsData[args.Num][args.Shard] {
				reply.Data[k] = v
			}

			reply.Seq = make(map[int64]int64)
			for k, v := range kv.oldshardsSeq[args.Num][args.Shard] {
				reply.Seq[k] = v
			}

			reply.Num = args.Num
			reply.Shard = args.Shard
			reply.Err = OK
			return
		}
	}

	reply.Err = ErrWrongGroup

}

// the tester calls Kill() when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
func (kv *ShardKV) Kill() {
	atomic.StoreInt32(&kv.dead, 1)
	kv.rf.Kill()
	// Your code here, if desired.
}

func (kv *ShardKV) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardmaster.
//
// pass masters[] to shardmaster.MakeClerk() so you can send
// RPCs to the shardmaster.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use masters[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int, masters []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.masters = masters

	// Your initialization code here.

	// Use something like this to talk to the shardmaster:
	// kv.mck = shardmaster.MakeClerk(kv.masters)

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	for i := 0; i < shardmaster.NShards; i++ {
		kv.db[i] = make(map[string]string)
		kv.clients[i] = make(map[int64]int64)
	}
	kv.channels = make(map[int]chan Op)
	kv.lastApplied = 0
	kv.configs = make([]shardmaster.Config, 1)
	kv.configs[0].Groups = map[int][]string{}
	kv.configs[0].Num = 0
	kv.pdclient = shardmaster.MakeClerk(kv.masters)

	kv.availableShards = make(map[int]bool)
	kv.requiredShards = make(map[int]bool)
	kv.garbageList = make(map[int]map[int]bool)
	// we need to store all old data, and use gc RPC to clean them !!!!
	kv.oldshards = make(map[int]map[int]bool)
	kv.oldshardsSeq = make(map[int]map[int]map[int64]int64)
	kv.oldshardsData = make(map[int]map[int]map[string]string)
	kv.readSnapshotForInit()

	if kv.maxraftstate != -1 {
		go kv.doSnapshot()
	}
	go kv.pullShards()
	go kv.pullConfig()
	go kv.processLog()
	go kv.sendGCRequest()
	return kv
}

func (kv *ShardKV) readSnapshotForInit() {
	snapshot, lastIncludedIndex := kv.rf.GetSnapshot()
	if snapshot != nil && len(snapshot) >= 1 {
		r := bytes.NewBuffer(snapshot)
		d := labgob.NewDecoder(r)

		if d.Decode(&kv.db) != nil ||
			d.Decode(&kv.clients) != nil ||
			d.Decode(&kv.configs) != nil ||
			d.Decode(&kv.oldConfig) != nil ||
			d.Decode(&kv.availableShards) != nil ||
			d.Decode(&kv.oldshards) != nil ||
			d.Decode(&kv.requiredShards) != nil ||
			d.Decode(&kv.oldshardsData) != nil ||
			d.Decode(&kv.oldshardsSeq) != nil ||
			d.Decode(&kv.garbageList) != nil {

			log.Fatalf("Unable to read persisted snapshot")
		}

		kv.lastApplied = lastIncludedIndex
	}
}

func (kv *ShardKV) pullConfig() {
	for !kv.killed() {
		//only leader can take the configuration
		if _, isLeader := kv.rf.GetState(); isLeader {
			nextNum := kv.latestConfig().Num + 1
			cfg := kv.pdclient.Query(nextNum)
			kv.mu.Lock()
			// first condition: add condition here to reduce useless log in Raft
			// second condition: make sure the migration is completed one by one
			if cfg.Num == kv.latestConfig().Num+1 && len(kv.requiredShards) == 0 {
				kv.mu.Unlock()
				op := Op{
					OpType: KvOp_Config,
					Config: cfg}
				kv.rf.Start(op)
			} else {
				kv.mu.Unlock()
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (kv *ShardKV) pullShards() {
	for !kv.killed() {
		//only leader can pull shards
		_, isLeader := kv.rf.GetState()
		kv.mu.Lock()
		if isLeader && len(kv.requiredShards) != 0 {
			// make a wait group here
			neededShards := make(map[int]bool)
			for k, v := range kv.requiredShards {
				neededShards[k] = v
			}
			oldConfig := kv.oldConfig.Num

			kv.mu.Unlock()

			wg := sync.WaitGroup{}
			wg.Add(len(neededShards))
			for k, _ := range neededShards {
				// TODO: add pull shards logic
				go func(shard int) {
					gid := kv.configs[oldConfig].Shards[shard]
					group := kv.configs[oldConfig].Groups[gid]
					for i := 0; i < len(group); i++ {
						srv := kv.make_end(group[i])
						args := GetMigrationArgs{
							Num:   kv.configs[oldConfig].Num,
							Shard: shard}

						reply := GetMigrationReply{}
						reply.Data = make(map[string]string)
						reply.Seq = make(map[int64]int64)
						ok := srv.Call("ShardKV.GetMigration", &args, &reply)

						op := Op{
							OpType:         KvOp_Migration,
							MigrationReply: reply}
						if ok && reply.Err == OK {
							kv.rf.Start(op)
							break
						}
					}
					wg.Done()
				}(k)
			}

			wg.Wait()
			DPrintf("Pull shards done")
			// waitgroup done
		} else {
			kv.mu.Unlock()
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Println("Thread killed")
}

func (kv *ShardKV) createSnapshot() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(kv.db)
	e.Encode(kv.clients)
	e.Encode(kv.configs)
	e.Encode(kv.oldConfig)
	e.Encode(kv.availableShards)
	e.Encode(kv.oldshards)
	e.Encode(kv.requiredShards)
	e.Encode(kv.oldshardsData)
	e.Encode(kv.oldshardsSeq)
	e.Encode(kv.garbageList)
	data := w.Bytes()
	lastApplied := kv.lastApplied
	//	DPrintf("Server %v at group %v generates snapshot at log intex %v", kv.me, kv.gid, lastApplied)
	kv.rf.GenerateSnapshot(data, lastApplied)
}

func (kv *ShardKV) doSnapshot() {
	for !kv.killed() {
		if kv.rf.GetRaftStateSize() > kv.maxraftstate {
			kv.mu.Lock()
			kv.createSnapshot()
			kv.mu.Unlock()

		}
		time.Sleep(20 * time.Millisecond)
	}

	fmt.Println("Thread killed")
}

func (kv *ShardKV) applyConfig(op *Op, msg *raft.ApplyMsg) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	DPrintf("Server %v at group %v apply config %v", kv.me, kv.gid, op.Config.Num)
	// apply config one by one

	if op.Config.Num == kv.latestConfig().Num+1 && len(kv.requiredShards) == 0 {

		if op.Config.Num == 1 {
			kv.oldConfig = kv.latestConfig()
			kv.configs = append(kv.configs, op.Config)
			newConfig := kv.latestConfig()
			for i := 0; i < shardmaster.NShards; i++ {
				if newConfig.Shards[i] == kv.gid {
					kv.availableShards[i] = true
				}
			}
		} else {
			kv.oldConfig = kv.latestConfig()
			kv.requiredShards = make(map[int]bool)

			kv.configs = append(kv.configs, op.Config)
			newConfig := kv.latestConfig()
			// use temp variable
			availableShards := make(map[int]bool)
			// old configuration matches required shards
			// how to manage pull data progress?
			kv.oldshards[kv.oldConfig.Num] = make(map[int]bool)
			kv.oldshardsSeq[kv.oldConfig.Num] = make(map[int]map[int64]int64)
			kv.oldshardsData[kv.oldConfig.Num] = make(map[int]map[string]string)
			// we don't need to rebalance for the first config

			// divide current data into three parts:
			// oldshards, requiredshards and db
			for i := 0; i < shardmaster.NShards; i++ {
				if newConfig.Shards[i] == kv.gid {
					// if the shards i serve does not exist, i need it
					if _, ok := kv.availableShards[i]; !ok {
						//	DPrintf("Server %v at group %v need: %v",  kv.me, kv.gid, i)
						kv.requiredShards[i] = true
					} else {
						// otherwise i can still serve it
						//	DPrintf("Server %v at group %v still available: %v",  kv.me, kv.gid, i)
						availableShards[i] = true
					}
					// delete the processed available shards
					delete(kv.availableShards, i)
				}
			}
			// the remaining shards are old shards
			// I am not responsible for those shards anymore.
			for shard, _ := range kv.availableShards {
				kv.oldshards[kv.oldConfig.Num][shard] = true
				kv.oldshardsData[kv.oldConfig.Num][shard] = make(map[string]string)
				kv.oldshardsSeq[kv.oldConfig.Num][shard] = make(map[int64]int64)
			}
			// update old shards data and user request id
			for shard, _ := range kv.oldshards[kv.oldConfig.Num] {
				// get old data
				// use deep copy
				for k, v := range kv.db[shard] {
					kv.oldshardsData[kv.oldConfig.Num][shard][k] = v
				}
				// clean data
				// let gc recycles data
				kv.db[shard] = make(map[string]string)
				// get old seq number
				// use deep copy
				for k, v := range kv.clients[shard] {
					kv.oldshardsSeq[kv.oldConfig.Num][shard][k] = v
				}
				// clean seq
				// let gc recycles data
			}

			// update the new available shards
			kv.availableShards = availableShards
		}

	}
	// remeber to update the lastapplied index :)
	if kv.lastApplied < msg.CommandIndex {
		kv.lastApplied = msg.CommandIndex
	}

}

func (kv *ShardKV) applyMigration(op *Op, msg *raft.ApplyMsg) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	DPrintf("Server %v at %v receives shard %v at config %v", kv.me, kv.gid, op.MigrationReply.Shard, op.MigrationReply.Num)
	if op.MigrationReply.Num != kv.oldConfig.Num {
		return
	}
	// make shard available
	delete(kv.requiredShards, op.MigrationReply.Shard)
	kv.availableShards[op.MigrationReply.Shard] = true

	kv.db[op.MigrationReply.Shard] = make(map[string]string)
	for k, v := range op.MigrationReply.Data {
		kv.db[op.MigrationReply.Shard][k] = v
	}

	// check the timestamp, be careful
	//if _, ok := kv.clients[op.MigrationReply.Shard]; !ok {
	//	kv.clients[op.MigrationReply.Shard] = make(map[int64]int64)
	//}
	for k, v := range op.MigrationReply.Seq {
		timeStamp, ok := kv.clients[op.MigrationReply.Shard][k]
		if ok && timeStamp > v {
			kv.clients[op.MigrationReply.Shard][k] = timeStamp
		} else {
			kv.clients[op.MigrationReply.Shard][k] = v
		}
	}

	if kv.lastApplied < msg.CommandIndex {
		kv.lastApplied = msg.CommandIndex
	}
	if _, ok := kv.garbageList[op.MigrationReply.Num]; !ok {
		kv.garbageList[op.MigrationReply.Num] = make(map[int]bool)
	}
	kv.garbageList[op.MigrationReply.Num][op.MigrationReply.Shard] = true

}

func (kv *ShardKV) applyGarbageCollection(op *Op, msg *raft.ApplyMsg) {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	needClean := true
	oldConfig, configOK := kv.oldshards[op.GCNum]
	if !configOK {
		needClean = false
	}

	_, shardOK := oldConfig[op.GCShard]

	if !shardOK {
		needClean = false
	}

	if needClean {
		DPrintf("Server %v at group %v GC CLEAN config %v shard %v\n", kv.me, kv.gid, op.GCNum, op.GCShard)
		delete(kv.oldshards[op.GCNum], op.GCShard)
		delete(kv.oldshardsData[op.GCNum], op.GCShard)
		delete(kv.oldshardsSeq[op.GCNum], op.GCShard)
	}

	if kv.lastApplied < msg.CommandIndex {
		kv.lastApplied = msg.CommandIndex
	}

}

func (kv *ShardKV) applyUserRequest(op *Op, msg *raft.ApplyMsg) {
	kv.mu.Lock()

	hashVal := key2shard(op.Key)

	_, shardOk := kv.availableShards[hashVal]
	// check the shard first
	if !shardOk || op.ConfigNumber != kv.latestConfig().Num {
		op.Err = ErrWrongGroup

		ch, ok := kv.channels[msg.CommandIndex]
		delete(kv.channels, msg.CommandIndex)

		kv.mu.Unlock()
		_, isLeader := kv.rf.GetState()
		if ok && isLeader {
			ch <- *op
		}
		return
	}

	seq, seqExists := kv.clients[hashVal][op.Id]
	// check seqnumber
	if !seqExists || seq < op.SeqNum {

		DPrintf("Server %v applied log at index %v.", kv.me, msg.CommandIndex)
		switch op.OpType {
		case KvOp_Put:
			{
				kv.db[hashVal][op.Key] = op.Value
				op.Err = OK
				DPrintf("Op seq value:%v PUT(%v, %v)", op.SeqNum, op.Key, op.Value)
			}
		case KvOp_Get:
			{
				val, exists := kv.db[hashVal][op.Key]
				if exists {
					op.Err = OK
					op.Value = val
				} else {
					op.Err = ErrNoKey
				}
				DPrintf("Op seq value:%v Get(%v, %v) Err (%v)", op.SeqNum, op.Key, op.Value, op.Err)

			}
		case KvOp_Append:
			{
				val, exists := kv.db[hashVal][op.Key]
				if exists {
					kv.db[hashVal][op.Key] = val + op.Value
					DPrintf("Server %v at group %v config %v shard %v Op seq value:%v Append(%v, %v -> %v) Err (%v) client : %v",
						kv.me, kv.gid, kv.latestConfig().Num, hashVal, op.SeqNum, op.Key, val, kv.db[hashVal][op.Key], op.Err, op.Id)

				} else {
					kv.db[hashVal][op.Key] = op.Value
					DPrintf("Op seq value:%v Append(%v, %v) Err (%v)", op.SeqNum, op.Key, kv.db[hashVal][op.Key], op.Err)

				}
				op.Err = OK
			}
		}
		kv.clients[hashVal][op.Id] = op.SeqNum
		ch, ok := kv.channels[msg.CommandIndex]
		delete(kv.channels, msg.CommandIndex)

		if kv.lastApplied < msg.CommandIndex {
			kv.lastApplied = msg.CommandIndex
		}

		kv.mu.Unlock()

		_, isLeader := kv.rf.GetState()
		if ok && isLeader {
			ch <- *op
		}
		return
	}
	kv.mu.Unlock()
}

func (kv *ShardKV) applySnapshot(msg *raft.ApplyMsg) {
	kv.mu.Lock()
	defer kv.mu.Unlock()

	DPrintf("Server %v at %v applied snapshot from %v to %v", kv.me, kv.gid, kv.lastApplied, msg.LastIncludedIndex)
	r := bytes.NewBuffer(msg.Command.([]byte))
	d := labgob.NewDecoder(r)

	if d.Decode(&kv.db) != nil ||
		d.Decode(&kv.clients) != nil ||
		d.Decode(&kv.configs) != nil ||
		d.Decode(&kv.oldConfig) != nil ||
		d.Decode(&kv.availableShards) != nil ||
		d.Decode(&kv.oldshards) != nil ||
		d.Decode(&kv.requiredShards) != nil ||
		d.Decode(&kv.oldshardsData) != nil ||
		d.Decode(&kv.oldshardsSeq) != nil ||
		d.Decode(&kv.garbageList) != nil {

		log.Fatalf("Unable to read persisted snapshot")
	}
	kv.lastApplied = msg.LastIncludedIndex
}

func (kv *ShardKV) processLog() {
	for !kv.killed() {
		msg := <-kv.applyCh

		if msg.CommandValid {
			op := msg.Command.(Op)
			switch op.OpType {
			case KvOp_Config:
				kv.applyConfig(&op, &msg)
			case KvOp_Migration:
				kv.applyMigration(&op, &msg)
			case KvOp_GarbageCollection:
				kv.applyGarbageCollection(&op, &msg)
			default:
				kv.applyUserRequest(&op, &msg)
			}

		} else if msg.LastIncludedIndex > kv.lastApplied {
			kv.applySnapshot(&msg)
		}

	}

	fmt.Printf("server %v at group %v Thread killed\n", kv.me, kv.gid)
}

func (kv *ShardKV) sendGCRequest() {
	for !kv.killed() {
		kv.mu.Lock()

		list := []GarbageCollectionArgs{}
		// config id -> shard id
		for cfgid, shards := range kv.garbageList {
			for shard, _ := range shards {
				list = append(list, GarbageCollectionArgs{
					Num:   cfgid,
					Shard: shard})
			}
		}

		kv.mu.Unlock()

		wg := sync.WaitGroup{}
		wg.Add(len(list))

		for i := 0; i < len(list); i++ {

			go func(args GarbageCollectionArgs) {
				gid := kv.configs[args.Num].Shards[args.Shard]
				group := kv.configs[args.Num].Groups[gid]
				for i := 0; i < len(group); i++ {
					clk := kv.make_end(group[i])
					reply := GarbageCollectionReply{}
					ok := clk.Call("ShardKV.GarbageCollectionRPC", &args, &reply)
					if ok {
						if reply.Err == OK {
							delete(kv.garbageList[args.Num], args.Shard)

							break
						} else if reply.Err == Deleting {
							break
						}
					}
				}
				wg.Done()
			}(list[i])
		}
		wg.Wait()
		time.Sleep(50 * time.Millisecond)
	}
}

func (kv *ShardKV) GarbageCollectionRPC(args *GarbageCollectionArgs, reply *GarbageCollectionReply) {
	kv.mu.Lock()
	oldConfig, configOK := kv.oldshards[args.Num]

	if !configOK {
		reply.Err = OK

		DPrintf("Server %v at group %v clean gc rpc config %v shard %v: reply :ok (%v)", kv.me, kv.gid, args.Num, args.Shard, reply.Err)
		kv.mu.Unlock()
		return
	}

	_, shardOK := oldConfig[args.Shard]

	if !shardOK {
		reply.Err = OK

		DPrintf("Server %v at group %v clean gc rpc config %v shard %v: reply :ok (%v)", kv.me, kv.gid, args.Num, args.Shard, reply.Err)
		kv.mu.Unlock()
		return
	}

	op := Op{
		OpType:  KvOp_GarbageCollection,
		GCNum:   args.Num,
		GCShard: args.Shard}
	kv.mu.Unlock()

	_, _, isLeader := kv.rf.Start(op)
	if isLeader {

		reply.Err = Deleting
		DPrintf("Server %v at group %v clean gc rpc config %v shard %v: reply :deleting (%v)", kv.me, kv.gid, args.Num, args.Shard, reply.Err)
		return
	}

	reply.Err = ErrWrongLeader
	DPrintf("Server %v at group %v clean gc rpc config %v shard %v: reply : wrongleader (%v)", kv.me, kv.gid, args.Num, args.Shard, reply.Err)
}
