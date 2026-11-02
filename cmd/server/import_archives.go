package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Tencent/WeKnora/internal/container"
	"github.com/Tencent/WeKnora/internal/envfile"
	"github.com/Tencent/WeKnora/internal/handler"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/runtime"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// localArchiveImporter narrows the knowledge service down to the bulk local
// import, so the subcommand depends on one method instead of the full
// KnowledgeService interface.
type localArchiveImporter interface {
	ImportLocalArchives(ctx context.Context, kbID, dir string) (*types.LocalArchiveImportReport, error)
}

// runImportArchives implements the `import-archives` subcommand: an
// operator-only bulk import that reads every package under a container-local
// directory (usually a host folder mounted at /app/import) and imports it
// into one knowledge base through the same pipeline as the zip upload, but
// without the HTTP upload caps — packages are bounded by the relaxed,
// finite LOCAL_IMPORT_* limits instead. A package is a .zip file or a
// directory whose contents look like an extracted zip (so folders can be
// mounted and imported directly, with no zip step); if the mount itself
// holds the extracted content, it is imported as one package and kept in
// place. Processed packages move to imported/ (success) or failed/ (any
// document failed) under the import directory, so re-running the command
// resumes with untouched packages.
//
// Usage:
//
//	./WeKnora import-archives --dir /app/import --kb-id <knowledge-base-id>
//
// Exit codes: 0 all packages imported, 1 some package failed (or the
// service failed to start), 2 usage error.
func runImportArchives(args []string) int {
	// Mirror the server's startup configuration loading so the subcommand
	// sees the same env/db/storage settings inside the container.
	if err := envfile.Load(handler.Edition == "lite"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	logger.ConfigureFromEnv()

	fs := flag.NewFlagSet("import-archives", flag.ContinueOnError)
	dirFlag := fs.String("dir", "", "包含待导入 .zip 或已解压目录的路径（容器内，如 /app/import）")
	kbFlag := fs.String("kb-id", "", "目标知识库 ID（必填）")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "用法: WeKnora import-archives --dir <目录> --kb-id <知识库ID>\n\n"+
			"扫描目录下的所有 .zip 与包含文档的子目录（跳过 imported/、failed/ 与隐藏目录），逐个导入指定知识库。\n"+
			"目录结构与 zip 解压后一致的文件夹可直接作为包导入，无需压缩；若导入目录本身就是要导入的内容，\n"+
			"将整体作为一个包导入并保留原位。包体积预算由 LOCAL_IMPORT_* 环境变量控制，默认为放宽后的有限上限。\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dirFlag == "" || *kbFlag == "" {
		fs.Usage()
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "未知参数: %v\n", fs.Args())
		fs.Usage()
		return 2
	}

	// SIGINT/SIGTERM stop the run between packages; the package in flight
	// finishes, untouched packages stay in place for the next run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	c := container.BuildContainer(runtime.GetContainer())
	var runErr error
	invokeErr := c.Invoke(func(kgService interfaces.KnowledgeService) {
		importer, ok := kgService.(localArchiveImporter)
		if !ok {
			runErr = fmt.Errorf("知识库服务不支持本地批量导入")
			return
		}
		report, err := importer.ImportLocalArchives(ctx, *kbFlag, *dirFlag)
		if err != nil {
			runErr = err
			return
		}
		printLocalArchiveReport(report)
		if report.FailedZips > 0 || report.FailedDirs > 0 {
			runErr = fmt.Errorf("%d 个包导入失败", report.FailedZips+report.FailedDirs)
		}
	})
	if invokeErr != nil {
		logger.Errorf(ctx, "Failed to initialize services for import-archives: %v", invokeErr)
		fmt.Fprintf(os.Stderr, "服务初始化失败: %v\n", invokeErr)
		return 1
	}
	if runErr != nil {
		fmt.Fprintf(os.Stderr, "导入未全部成功: %v\n", runErr)
		return 1
	}
	return 0
}

// printLocalArchiveReport writes the per-package summary to stdout so the
// invoking shell (docker compose exec ...) sees the outcome directly.
func printLocalArchiveReport(report *types.LocalArchiveImportReport) {
	fmt.Printf("本地批量导入完成: 目录=%s 知识库=%s\n", report.Dir, report.KnowledgeBaseID)
	for _, note := range report.Notes {
		fmt.Printf("  [note] %s\n", note)
	}
	for _, item := range report.Items {
		line := fmt.Sprintf("  [%s/%s] %s", item.Kind, item.Status, item.Path)
		if item.Status == types.LocalArchiveStatusImported {
			line += fmt.Sprintf(" created=%d duplicate=%d skipped_files=%d",
				item.Created, item.Duplicate, item.SkippedFiles)
		} else if item.Error != "" {
			line += fmt.Sprintf(" error=%q", item.Error)
		}
		if item.MovedTo != "" {
			line += fmt.Sprintf(" -> %s", item.MovedTo)
		}
		fmt.Println(line)
		// Surface per-document warnings (e.g. parse-budget overruns) so the
		// operator sees them without opening the stored report.
		if item.Report != nil {
			for i := range item.Report.Items {
				for _, warn := range item.Report.Items[i].Warnings {
					fmt.Printf("    [warn] %s: %s\n", item.Report.Items[i].Title, warn)
				}
			}
		}
	}
	fmt.Printf("合计: imported(zip=%d dir=%d) failed(zip=%d dir=%d)\n",
		report.ImportedZips, report.ImportedDirs, report.FailedZips, report.FailedDirs)
}
