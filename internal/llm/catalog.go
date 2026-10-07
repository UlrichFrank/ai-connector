package llm

import (
	"fmt"
	"sort"
	"strings"

	"github.com/philipparndt/ai-connector/internal/models"
)

// Chat is a chat model of the catalogue: an MLX 4-bit checkpoint of mlx-community, pinned like the other models
// (repository, revision, every file's size and SHA-256).
type Chat struct {
	models.Model        // Name: how vboard shows it, e.g. "Qwen3.6-35B-A3B (4-bit)"
	Short        string // --llm-model name, e.g. "qwen3.6-35b-a3b"
	MinRAM       int64  // the default from this much memory on
	About        string
}

const gb = int64(1) << 30

// The catalogue, small to large. All are Qwen3.5/3.6 (gated delta-net + attention layers), call tools well and
// answer in the language they are asked in; thinking is off (the answer comes at once).
var (
	Qwen35_4B = Chat{Short: "qwen3.5-4b", MinRAM: 0, About: "dense 4B, for Macs with 8–15 GB",
		Model: models.Model{Name: "Qwen3.5-4B (4-bit)", Repo: "mlx-community/Qwen3.5-4B-4bit", Rev: "0e7ffd5c629ef7719d4cbc04069232580bfa9d9c",
			Files: []models.File{
				{Path: "config.json", Size: 3366, SHA256: "f3efc81b2ea8d96a45301037d3ccccbcccdef44a961845c87f286aaddbc6eaaa"},
				{Path: "tokenizer.json", Size: 19989343, SHA256: "87a7830d63fcf43bf241c3c5242e96e62dd3fdc29224ca26fed8ea333db72de4"},
				{Path: "model.safetensors.index.json", Size: 101944, SHA256: "52e534c41f7b97708329c85f762e5882bf48bd5955a422c6ae74eba321e6048a"},
				{Path: "model.safetensors", Size: 3034300695, SHA256: "5fb9acd0246866381cf8c5c354c6db1019f6498eec4ccb4f5edcc71ffeacb2db"},
			}}}
	Qwen35_9B = Chat{Short: "qwen3.5-9b", MinRAM: 16 * gb, About: "dense 9B, for Macs with 16–35 GB",
		Model: models.Model{Name: "Qwen3.5-9B (4-bit)", Repo: "mlx-community/Qwen3.5-9B-4bit", Rev: "8b2b98c00a6b4d291155e4890773ca8f769aee53",
			Files: []models.File{
				{Path: "config.json", Size: 3331, SHA256: "a96942cb6a8a1d3f1d17514d81a1925d04362a6a3233b389d13012211baaa9f8"},
				{Path: "tokenizer.json", Size: 19989343, SHA256: "87a7830d63fcf43bf241c3c5242e96e62dd3fdc29224ca26fed8ea333db72de4"},
				{Path: "model.safetensors.index.json", Size: 123592, SHA256: "dd023913fb87cfdae27fb11dcf695117c925833796ccac3c64117d6652d8ff1e"},
				{Path: "model-00001-of-00002.safetensors", Size: 5349771222, SHA256: "a68b87558c6ef43f74c2bd63ce7e9092ceddc3101f3def0030774bae5f42aadd"},
				{Path: "model-00002-of-00002.safetensors", Size: 600449850, SHA256: "b0a770bf8469c7f3f18756a0e0283f1c1174344a83e059a4e483f6af4907352d"},
			}}}
	Qwen36_35BA3B = Chat{Short: "qwen3.6-35b-a3b", MinRAM: 36 * gb, About: "Mixture of Experts, 35B with 3B active: fast and strong, for Macs with 36 GB or more",
		Model: models.Model{Name: "Qwen3.6-35B-A3B (4-bit)", Repo: "mlx-community/Qwen3.6-35B-A3B-4bit", Rev: "38740b847e4cb78f352aba30aa41c76e08e6eb46",
			Files: []models.File{
				{Path: "config.json", Size: 23591, SHA256: "a822a9e48b0aafbe3144ec37d4fb067e178ed96615ce6e4420b3149893cc5767"},
				{Path: "tokenizer.json", Size: 19989343, SHA256: "87a7830d63fcf43bf241c3c5242e96e62dd3fdc29224ca26fed8ea333db72de4"},
				{Path: "model.safetensors.index.json", Size: 215755, SHA256: "0b28df60e33753a14e816d3b31577ae2c93884c58430a4a6de6ae9ea483842ea"},
				{Path: "model-00001-of-00004.safetensors", Size: 5288196018, SHA256: "09f3e6ecb0b7af6e6a38bc8169a134c821b0924c2679b2bb8f4426ad38d032b8"},
				{Path: "model-00002-of-00004.safetensors", Size: 5368472749, SHA256: "31dcdb1c49eebdb1505bd14e3cb33f9cf900bd2546b638f2464694ae763a033f"},
				{Path: "model-00003-of-00004.safetensors", Size: 5368324139, SHA256: "3e66de06a1f03dade16a612a368cfce4a4c9caa4efd7d28185454384082cec03"},
				{Path: "model-00004-of-00004.safetensors", Size: 4377211365, SHA256: "a5d0cf03519c26f8b506df6b0ba60526e5c08c8cea22d0c21ce92950e58a5422"},
			}}}
	// dense 27B: stronger than the 35B-A3B on hard questions, but slow (every weight is read for every token); only
	// on request (--llm-model qwen3.6-27b)
	Qwen36_27B = Chat{Short: "qwen3.6-27b", MinRAM: 1 << 62, About: "dense 27B: careful but slow, on request, for 32 GB or more",
		Model: models.Model{Name: "Qwen3.6-27B (4-bit)", Repo: "mlx-community/Qwen3.6-27B-4bit", Rev: "c000ac2c2057d94be3fa931000c31723aac53282",
			Files: []models.File{
				{Path: "config.json", Size: 4573, SHA256: "ede24666ac51e6d5ab948a8a1e6c72fc6effd941ba3aabb6dd942eb517c78043"},
				{Path: "tokenizer.json", Size: 19989343, SHA256: "87a7830d63fcf43bf241c3c5242e96e62dd3fdc29224ca26fed8ea333db72de4"},
				{Path: "model.safetensors.index.json", Size: 218281, SHA256: "13b840162b4cb35c66fef7df072f7dbb4717908204364f5e5d9f9655a2758fa8"},
				{Path: "model-00001-of-00003.safetensors", Size: 5343268752, SHA256: "2689680915661f040c50c35244d08b336def279e509e1ca11873f8dd1b0e7ce0"},
				{Path: "model-00002-of-00003.safetensors", Size: 5354185100, SHA256: "a46183727b2c5cd16613fd8395f5e9e7cc4ad679644558cc87c9406fe334463d"},
				{Path: "model-00003-of-00003.safetensors", Size: 5357087747, SHA256: "ac23bf70b1f239a040921d6f93770d74176fd435dbf44e42317053d06c68d702"},
			}}}

	Catalogue = []Chat{Qwen35_4B, Qwen35_9B, Qwen36_35BA3B, Qwen36_27B}
)

// Default is the catalogue model for a machine with mem bytes of memory: the largest whose MinRAM it has.
func Default(mem int64) Chat {
	best := Catalogue[0]
	for _, c := range Catalogue {
		if mem >= c.MinRAM && c.MinRAM >= best.MinRAM {
			best = c
		}
	}
	return best
}

// Fits: the model's weights leave room on a machine with mem bytes (Metal may wire about three quarters of the
// memory; the classifier, the embedder and the KV cache need their share).
func (c Chat) Fits(mem int64) bool { return mem == 0 || c.Size()*100/70 < mem }

// Find looks a catalogue model up by its short name, its repository or its name.
func Find(name string) (Chat, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, c := range Catalogue {
		if n == c.Short || n == strings.ToLower(c.Repo) || n == strings.ToLower(c.Name) ||
			n == strings.ToLower(c.Repo[strings.LastIndex(c.Repo, "/")+1:]) {
			return c, true
		}
	}
	return Chat{}, false
}

// CatalogueHelp lists the catalogue for messages and --help.
func CatalogueHelp() string {
	var b strings.Builder
	cs := append([]Chat(nil), Catalogue...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Size() < cs[j].Size() })
	for _, c := range cs {
		fmt.Fprintf(&b, "  %-16s %-38s %7s  %s\n", c.Short, c.Repo, models.Human(c.Size()), c.About)
	}
	return b.String()
}
