// Package migrations 内嵌仓库文档约定的 migrations/ 目录下的版本化 SQL 文件，
// 供适配器在启动时按序应用。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
