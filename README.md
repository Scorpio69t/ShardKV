## Preview
该项目实现了MIT 6.824(2020)中的四个lab，包括一些Chanllenge的部分，其中服务器之间通过RPC进行通信，lab2-lab4是一个层层递进的关系
## lab1 MapReduce
有多台服务器，其中一台作为server负责任务的分发以及失效worker的处理，其余都作为worker来调用Map和Reduce函数处理读写文件。  
注意：  
如果worker没有在合理的时间内完成任务(10s)，master应该将任务交给另一个worker来处理；  
worker应该将Map输出的intermediate放在当前目录下的文件中，稍后worker可以将其作为Reduce任务的输入读取

## lab2 Raft
raft是一个分布式系统的一致性协议，同时还有一定程度的容错  
复制服务通过在多个复制服务器上存储其状态(即数据)的完整副本来实现容错。复制允许服务继续运行，即使它的一些服务器遇到故障(崩溃或中断或不稳定的网络)。  
挑战在于，失败可能导致副本保存数据的不同副本。
Raft将客户端请求组织成一个序列，称为日志，并确保所有副本服务器看到相同的日志。每个副本按日志顺序执行客户端请求，并将其应用于服务状态的本地副本。由于所有活动副本都看到相同的日志内容，因此它们都以相同的顺序执行相同的请求，从而继续具有相同的服务状态。如果服务器出现故障，但后来恢复了，raft会负责更新日志。只要至少大多数服务器还活着并且可以相互通信，Raft就会继续运行；否则 Raft将不会取得任何进展，但一旦大多数人能够再次进行通信，它就会从中断的地方重新开始。
主要RPC：  
### StartAppendEntries(is bool) 
由leader调用，参数用于区分初始心跳和后续的AppendEntries操作；  
对每个peer(除自己外)，调用go routine并行发送请求，  
如果nextIndex[id] >= rf.LastIncludedIndex + 1，则发送AppendEntries RPC（需要添加日志条目）
否则发送InstallSnapshot RPC安装快照（更新nextIndex数组）
### AppendEntries(args, reply) 
follower收到的关于leader调用的RPC。  
对于收到的每条Entry，如果发生冲突（index相同而term不同），则删除peer中该index及之后的日志，然后替换成Entries，否则如不在日志中则直接添加
### RequestVote(args)
peer收到candidate的投票请求。  
如果peer也是candidate，则会比较term，若对方的term比自己大，自己转变为follower状态;  
如果对方term比自己大并且自己还没投票（或者恰好投给了对方），同时对方最后日志的信息至少跟自己一样新（term相同，index相等或更大，或term更大）;  
回复的term是两者term的较大值。
### InstallSnapshot(args, reply)
由leader调用，如果nextIndex[id] < rf.LastIncludedIndex + 1,说明nextIndex数组没有及时更新，该方法会更新peer对应的lastIncludedIndex和lastIncludedTerm、lastApplied  
删除lastIncluedIndex之前的日志
## lab3 KVRaft
构建了一个基于Raft的kv存储系统，客户会对服务器发出RPC请求，通过raft达成共识后会返回给用户对应的结果，主要支持的操作有Get(获取键值)、Put(替换键值)以及Append操作(追加键值) ，为了防止日志无限制的增长，还实现了快照功能，使得服务器可以丢弃之前的日志，当服务器重新启动后可以通过安装快照来迅速恢复之前的状态 
Client的结构定义
```
type Clerk struct {
	servers []*labrpc.ClientEnd
	// You will have to modify this struct.  
	lastLeader int        //上一个联络的leader  
	seqNumber  int64      //执行命令的编号  
	id         int64      //客户的编号  
	mu         sync.Mutex //互斥锁  
}
```
server中重要方法
### Get()及PutAppend()
Client发出请求后调用RPC，服务器会调用某个服务器中对应的方法  
判断服务器是否为leader同时取出对应客户Id的序列号判断是否有效(>=args中序列号，说明该操作已被处理)  
封装op,调用kv.rf.start(op)加入日志，在对应index(lastLogIndex+1)处创建队列并监听（监听成功更新reply），超时返回ErrWrongLeader，更新reply的值
### processLog()
服务器启动后便会启动go routine轮询  
从消息队列取出消息（来源于raft）  
来自applyLog()  
若操作有效(序列号大于客户当前序列号)，根据操作类型对本地db进行操作，更新客户对应的序列号  
取出对应index处的消息队列，将op放入
来自installSnapshot()  
对lastApplied更新
### doSnapshot()
服务器启动后便会启动go routine轮询  
当stateSize超过最大值，调用raft中GenerateSnapshot生成快照

