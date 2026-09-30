package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/freezeChen/frz-tools/api/v1"
	"github.com/freezeChen/frz-tools/internal/adapters/config"
	"github.com/freezeChen/frz-tools/internal/domain"

	"gopkg.in/yaml.v3"
)

// initDefaultTemplate 是 opsctl init 默认生成的完整文本。
func initDefaultTemplate() string { return initTemplateBody + initRemoteCommented }

// initRemoteTemplate 是 opsctl init --remote 生成的完整文本。
func initRemoteTemplate() string { return initTemplateBody + initRemoteActive }

// 默认模板必须直接通过 config.Load：init 的承诺是「生成即合法」（迭代 6 规格 §9.1
// 第 1 条）。模板写坏却让用户去 config validate 里撞墙，等于把单测的活儿推给用户。
func TestInitTemplateLoadsAsValidConfig(t *testing.T) {
	if _, err := config.LoadBytes([]byte(initDefaultTemplate())); err != nil {
		t.Fatalf("init 默认模板应当能通过 config.Load：%v", err)
	}
}

// --remote 模板在填入真实证书之前必须**通不过**校验，而且失败消息要逐条指出
// 缺的是哪个文件——这正是它占位路径存在的意义（迭代 6 规格 D2）。
func TestInitRemoteTemplateFailsUntilCertificatesExist(t *testing.T) {
	_, err := config.LoadBytes([]byte(initRemoteTemplate()))
	if domain.CodeOf(err) != v1.CodeConfigInvalid {
		t.Fatalf("--remote 模板在证书就位前应当是 CONFIG_INVALID，得到 %v", err)
	}
	for _, placeholder := range []string{
		"/etc/opsd/pki/server.crt",
		"/etc/opsd/pki/server.key",
		"/etc/opsd/pki/ca.crt",
	} {
		if !strings.Contains(domain.MessageOf(err), placeholder) {
			t.Fatalf("失败消息应当逐条指向缺失的证书文件 %s：%v", placeholder, err)
		}
	}
}

// 单一来源对策（迭代 6 规格 D2）：模板内嵌后与 opsd.example.yaml 是两份文本，会漂移。
// 这条测试解析两份 YAML、逐层比较键集合（注释不计入——注释本来就不在节点树里），
// 漂移让测试红，而不是让用户先发现。
func TestInitTemplateMatchesExampleKeySet(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "opsd.example.yaml"))
	if err != nil {
		t.Fatalf("read opsd.example.yaml: %v", err)
	}
	diffs := diffKeySets(parseYAMLValue(t, initDefaultTemplate()), parseYAMLValue(t, string(example)), "")
	for _, path := range diffs.onlyInA {
		t.Errorf("键 %q 只在 init 模板里，opsd.example.yaml 没有", path)
	}
	for _, path := range diffs.onlyInB {
		t.Errorf("键 %q 只在 opsd.example.yaml 里，init 模板没有", path)
	}
	for _, path := range diffs.structural {
		t.Errorf("键 %q 的结构在两份文本里不同", path)
	}
}

// --remote 变体只应该在默认模板之上多出 remote 段：其余键集合与默认模板完全一致。
func TestInitRemoteTemplateOnlyAddsRemoteSection(t *testing.T) {
	diffs := diffKeySets(parseYAMLValue(t, initRemoteTemplate()), parseYAMLValue(t, initDefaultTemplate()), "")
	if len(diffs.onlyInB) != 0 {
		t.Fatalf("--remote 模板不该比默认模板少键：%v", diffs.onlyInB)
	}
	if len(diffs.structural) != 0 {
		t.Fatalf("--remote 模板不该有 remote 之外的结构差异：%v", diffs.structural)
	}
	for _, path := range diffs.onlyInA {
		if path != "remote" && !strings.HasPrefix(path, "remote.") {
			t.Errorf("--remote 模板不该在 remote 之外多出键 %q", path)
		}
	}
	if len(diffs.onlyInA) == 0 {
		t.Fatal("--remote 模板应当多出 remote 段，键集合却与默认模板一致")
	}
}

// parseYAMLValue 把 YAML 文本解析成节点树（取文档根）。
func parseYAMLValue(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("parse yaml: %v", err)
	}
	if len(doc.Content) == 0 {
		t.Fatal("yaml 文档为空")
	}
	return doc.Content[0]
}

// mappingKeys 把映射节点展开成「键 → 值节点」。
func mappingKeys(node *yaml.Node) map[string]*yaml.Node {
	keys := make(map[string]*yaml.Node, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys[node.Content[i].Value] = node.Content[i+1]
	}
	return keys
}

// keySetDiffs 是两棵 YAML 节点树的键集合差异。onlyInA / onlyInB 是只在其中一边
// 出现的键路径；structural 是两边都存在、但结构种类不同（映射 vs 标量等）的路径。
type keySetDiffs struct {
	onlyInA    []string
	onlyInB    []string
	structural []string
}

// diffKeySets 逐层比较两份 YAML 的键集合：映射比较键名并逐键递归；序列按下标
// 配对递归（键集合的语义不关心序列长度）；标量是叶子。注释不进入节点树，天然不计入。
func diffKeySets(a, b *yaml.Node, path string) keySetDiffs {
	var diffs keySetDiffs
	merge := func(sub keySetDiffs) {
		diffs.onlyInA = append(diffs.onlyInA, sub.onlyInA...)
		diffs.onlyInB = append(diffs.onlyInB, sub.onlyInB...)
		diffs.structural = append(diffs.structural, sub.structural...)
	}
	join := func(key string) string {
		if path == "" {
			return key
		}
		return path + "." + key
	}

	if a.Kind == yaml.MappingNode && b.Kind == yaml.MappingNode {
		aKeys, bKeys := mappingKeys(a), mappingKeys(b)
		for key, value := range aKeys {
			if _, ok := bKeys[key]; !ok {
				diffs.onlyInA = append(diffs.onlyInA, join(key))
				continue
			}
			merge(diffKeySets(value, bKeys[key], join(key)))
		}
		for key := range bKeys {
			if _, ok := aKeys[key]; !ok {
				diffs.onlyInB = append(diffs.onlyInB, join(key))
			}
		}
		return diffs
	}

	if a.Kind == yaml.SequenceNode && b.Kind == yaml.SequenceNode {
		pairs := len(a.Content)
		if len(b.Content) < pairs {
			pairs = len(b.Content)
		}
		for i := 0; i < pairs; i++ {
			merge(diffKeySets(a.Content[i], b.Content[i], fmt.Sprintf("%s[%d]", path, i)))
		}
		return diffs
	}

	// 至少一边不是映射/序列：两边同为标量（哪怕值不同）不在键集合的管辖范围内，
	// 但一边是集合一边是标量说明结构本身漂移了，必须报出来。
	collection := func(n *yaml.Node) bool {
		return n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode
	}
	if collection(a) != collection(b) {
		diffs.structural = append(diffs.structural, join(
			fmt.Sprintf("（一边是 %s，另一边是 %s）", nodeKind(a), nodeKind(b))))
	}
	return diffs
}

func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "映射"
	case yaml.SequenceNode:
		return "序列"
	case yaml.ScalarNode:
		return "标量"
	default:
		return fmt.Sprintf("kind=%d", node.Kind)
	}
}
