// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// IO 后端抽象（P2.2a，零挂载访问层第一小步）。
//
// Guard 的五道校验（词法→白名单→openat 逐段解析→再前缀→黑名单）只回答
// 「这个路径可不可以动」，不回答「动了以后东西从哪来/到哪去」——后者
// 由 Backend 承担。目前唯一实现是 osBackend（宿主机直读，现状平迁）；
// P2.2c/d 落 dockerBackend：同名路径语义换成「借道容器 → docker cp tar 流」，
// Guard 的调用点与校验层零改动。
//
// 接口形状刻意贴着 os 原生函数（*os.File、fs.FileInfo、os.DirEntry）：
// 平迁期错误值不包装，errors.Is(err, fs.ErrNotExist) 等判定原样生效；
// docker 后端将来负责把自己的错误翻译成同一套 fs 语义。
package fsapi

import (
	"io/fs"
	"os"
)

// Backend 是 Guard 的 IO 手脚：入参全部是 Resolve 通过后的白名单内绝对路径。
// 实现方可以假定路径已被校验，但不得自行放宽（比如跟随软链到根外）。
type Backend interface {
	Stat(abs string) (fs.FileInfo, error)
	Lstat(abs string) (fs.FileInfo, error)
	ReadDir(abs string) ([]os.DirEntry, error)
	Open(abs string) (*os.File, error)
	CreateTemp(dir, pattern string) (*os.File, error)
	OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error)
	Mkdir(abs string, perm fs.FileMode) error
	Remove(abs string) error
	RemoveAll(abs string) error
	Rename(old, new string) error
}

// osBackend 是宿主机直读实现：逐函数转发 os 包，行为与抽接口前完全一致。
type osBackend struct{}

func (osBackend) Stat(abs string) (fs.FileInfo, error) { return os.Stat(abs) }

func (osBackend) Lstat(abs string) (fs.FileInfo, error) { return os.Lstat(abs) }

func (osBackend) ReadDir(abs string) ([]os.DirEntry, error) { return os.ReadDir(abs) }

func (osBackend) Open(abs string) (*os.File, error) { return os.Open(abs) }

func (osBackend) CreateTemp(dir, pattern string) (*os.File, error) {
	return os.CreateTemp(dir, pattern)
}

func (osBackend) OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}

func (osBackend) Mkdir(abs string, perm fs.FileMode) error { return os.Mkdir(abs, perm) }

func (osBackend) Remove(abs string) error { return os.Remove(abs) }

func (osBackend) RemoveAll(abs string) error { return os.RemoveAll(abs) }

func (osBackend) Rename(old, new string) error { return os.Rename(old, new) }
