package main

import "sync"

// safeClose 是外部插件的 close 纪律（从 clawhubadapter/safeclose.go 移植）。
//
// 内核的 internal/safeclose 外部插件拿不到，只能自己写。sync.Once 是
// 唯一无副作用的守卫：select 探查 channel 要么消费消息、要么阻塞，
// 都不能用来判断"关了没有"。
//
// 每个独立生命周期（stopCh、readerReady、每个 pendingResp）必须拥有
// 自己的 Once —— 共用一个全局 Once 会在第二次 close 时 panic。
func safeClose[T any](once *sync.Once, ch chan T) {
	once.Do(func() { close(ch) })
}
