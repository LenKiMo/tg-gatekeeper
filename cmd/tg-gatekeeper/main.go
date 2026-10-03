// Command tg-gatekeeper 是"看图选名字"入群验证机器人。
//
// 子命令：
//
//	run            启动机器人
//	check-config   启动前自检（配置/存储/题库/图片）
//	dataset        数据集导入与检查
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/LenKiMo/tg-gatekeeper/internal/app"
	"github.com/LenKiMo/tg-gatekeeper/internal/config"
	"github.com/LenKiMo/tg-gatekeeper/internal/dataset"
)

const usage = `tg-gatekeeper - 通用"看图选名字"入群验证机器人

用法:
  tg-gatekeeper run           -c configs/gatekeeper.yaml
  tg-gatekeeper check-config  -c configs/gatekeeper.yaml
  tg-gatekeeper dataset wiki  --api <api.php> --category <Category:条目分类> --out data/items.json [--thumb-width 600] [--label-field cnname] [--download-images data/images]
  tg-gatekeeper dataset json  --input raw.json --out data/items.json [--download-images data/images]
  tg-gatekeeper version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		must(runCmd(os.Args[2:]))
	case "check-config":
		must(checkConfigCmd(os.Args[2:]))
	case "dataset":
		must(datasetCmd(os.Args[2:]))
	case "version":
		fmt.Printf("tg-gatekeeper %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

// version 是构建版本号：源码构建显示下面这个值，发布流程用
// -ldflags "-X main.version=<tag>" 覆盖成实际 tag。
var version = "1.0.3"

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("c", "configs/gatekeeper.yaml", "配置文件路径")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	a, err := app.New(ctx, cfg)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

func checkConfigCmd(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ExitOnError)
	cfgPath := fs.String("c", "configs/gatekeeper.yaml", "配置文件路径")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("配置校验失败: %w", err)
	}
	fmt.Println("配置校验通过，开始自检：")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := app.CheckConfig(ctx, cfg); err != nil {
		return err
	}
	fmt.Println("自检全部通过。")
	return nil
}

func datasetCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法: tg-gatekeeper dataset <wiki|json> [flags]")
	}
	switch args[0] {
	case "wiki":
		return datasetWikiCmd(args[1:])
	case "json":
		return datasetJSONCmd(args[1:])
	default:
		return fmt.Errorf("未知的 dataset 子命令 %q（可选 wiki/json）", args[0])
	}
}

func datasetWikiCmd(args []string) error {
	fs := flag.NewFlagSet("dataset wiki", flag.ExitOnError)
	api := fs.String("api", "", "MediaWiki api.php 地址")
	category := fs.String("category", "", "分类名，例如 Category:Items")
	labelField := fs.String("label-field", "cnname", "信息框里作为标签的字段")
	imageField := fs.String("image-field", "image", "信息框里作为图片的字段")
	idField := fs.String("id-field", "filename", "信息框里作为稳定 ID 的字段")
	thumbWidth := fs.Int("thumb-width", 600, "wiki 缩略图宽度，0 表示用原图")
	pool := fs.String("pool", "default", "写入所有条目的选项池")
	minRarity := fs.Int("min-rarity", 0, "稀有度下限（0 表示不过滤）")
	maxPages := fs.Int("max-pages", 0, "最多抓取页面数（0 表示全部）")
	requireLabel := fs.Bool("require-label", true, "丢弃没有标签的页面")
	out := fs.String("out", "data/items.json", "输出文件")
	imgDir := fs.String("download-images", "", "把图片下载到该目录，并把数据集里的 image 改写为相对路径")
	ua := fs.String("user-agent", "", "抓取用的 UA（wiki 要求可识别的 UA）")
	fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	f, err := dataset.ImportFromWiki(ctx, dataset.WikiOptions{
		API:          *api,
		Category:     *category,
		LabelField:   *labelField,
		ImageField:   *imageField,
		IDField:      *idField,
		ThumbWidth:   *thumbWidth,
		Pool:         *pool,
		MinRarity:    *minRarity,
		MaxPages:     *maxPages,
		RequireLabel: *requireLabel,
		UserAgent:    *ua,
	})
	if err != nil {
		return err
	}
	if *imgDir != "" {
		ok, fail, err := f.DownloadImages(ctx, *imgDir, *ua)
		if err != nil {
			return err
		}
		fmt.Printf("图片下载完成：成功 %d，失败 %d（失败条目仍保留远程 URL，可随时重试）\n", ok, fail)
	}
	if err := f.Save(*out); err != nil {
		return err
	}
	printDatasetSummary(f, *out)
	return nil
}

func datasetJSONCmd(args []string) error {
	fs := flag.NewFlagSet("dataset json", flag.ExitOnError)
	input := fs.String("input", "", "输入 JSON（数组，或 {\"items\":[...]}）")
	out := fs.String("out", "data/items.json", "输出文件")
	imgDir := fs.String("download-images", "", "把图片下载到该目录")
	pool := fs.String("pool", "", "覆盖 pool（留空则沿用输入）")
	fs.Parse(args)

	if *input == "" {
		return fmt.Errorf("必须提供 --input")
	}
	raw, err := os.ReadFile(*input)
	if err != nil {
		return fmt.Errorf("读取输入失败: %w", err)
	}
	f, err := dataset.FromRawJSON(raw, *pool)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if *imgDir != "" {
		ok, fail, err := f.DownloadImages(ctx, *imgDir, "")
		if err != nil {
			return err
		}
		fmt.Printf("图片下载完成：成功 %d，失败 %d\n", ok, fail)
	}
	f.Source = "json:" + *input
	f.Generated = time.Now().Format(time.RFC3339)
	if err := f.Save(*out); err != nil {
		return err
	}
	printDatasetSummary(f, *out)
	return nil
}

func printDatasetSummary(f *dataset.File, out string) {
	labels := map[string]int{}
	pools := map[string]int{}
	for _, it := range f.Items {
		labels[it.Label]++
		pool := it.Pool
		if pool == "" {
			pool = "default"
		}
		pools[pool]++
	}
	dup := 0
	for _, n := range labels {
		if n > 1 {
			dup++
		}
	}
	fmt.Printf("数据集已写入 %s\n", out)
	fmt.Printf("· 条目 %d，去重标签 %d，重复标签 %d\n", len(f.Items), len(labels), dup)
	fmt.Printf("· 选项池 %v\n", pools)
	if len(f.Items) > 0 {
		fmt.Printf("· 示例：%s -> %s\n", f.Items[0].Label, f.Items[0].Image)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
