package gateway

import _ "embed"

// M0SSPrompt 是内置的「M0SS」人设全文，随二进制一起打包。
// 面板点「一键写入 M0SS 人设」时直接用它，省得手动粘贴。
//
//go:embed m0ss_prompt.txt
var M0SSPrompt string
