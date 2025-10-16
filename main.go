package main

import (
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	_ "modernc.org/sqlite"
)

var (
	englishDB      *sql.DB
	chineseDB      *sql.DB
	app            *tview.Application
	searchHistory  []string   // 搜索历史
	historyMutex   sync.Mutex // 保护搜索历史的并发访问
	maxHistorySize = 20       // 最多保存20条历史
)

// Word 表示一个单词的完整信息
type Word struct {
	Word        string
	Phonetic    string
	Definition  string
	Translation string
	Bnc         string
}

// cleanNewlines 清除字符串中的换行符
func cleanNewlines(s string) string {
	s = strings.ReplaceAll(s, "\\r\\n", "；")
	s = strings.ReplaceAll(s, "\\n", "；")
	s = strings.ReplaceAll(s, "\\r", "；")
	return strings.TrimSpace(s)
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// decompressGzipFile 解压缩gzip文件
func decompressGzipFile(gzFile, outputFile string) error {
	// 打开.gz文件
	f, err := os.Open(gzFile)
	if err != nil {
		return fmt.Errorf("无法打开文件 %s: %v", gzFile, err)
	}
	defer f.Close()

	// 创建gzip.Reader
	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("无法创建gzip.Reader: %v", err)
	}
	defer gr.Close()

	// 创建输出文件
	out, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("无法创建文件 %s: %v", outputFile, err)
	}
	defer out.Close()

	// 解压缩并写入
	if _, err := io.Copy(out, gr); err != nil {
		return fmt.Errorf("解压缩失败: %v", err)
	}

	return nil
}

func main() {
	// 检查并初始化数据库
	csvFile := "ecdict.csv"
	gzFile := "ecdict.csv.gz"
	englishDBFile := "english_chinese.db"
	chineseDBFile := "chinese_english.db"

	// 检查数据库文件是否存在
	_, errEnglish := os.Stat(englishDBFile)
	_, errChinese := os.Stat(chineseDBFile)

	if os.IsNotExist(errEnglish) || os.IsNotExist(errChinese) {
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("  📚 欢迎使用中英文词典")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println()
		fmt.Println("✨ 检测到这是首次运行，需要初始化数据库")
		fmt.Println("⏱️  预计需要 1-2 分钟，请耐心等待...")
		fmt.Println("💡 此操作仅需执行一次，后续启动将秒开！")
		fmt.Println()

		// 检查CSV文件是否存在
		if _, err := os.Stat(csvFile); os.IsNotExist(err) {
			// CSV文件不存在，检查.gz文件
			if _, err := os.Stat(gzFile); os.IsNotExist(err) {
				fmt.Printf("❌ 错误: 找不到 %s 或 %s 文件\n", csvFile, gzFile)
				return
			}

			// 解压缩.gz文件
			fmt.Println("📦 步骤 1/3: 解压缩词典数据...")
			if err := decompressGzipFile(gzFile, csvFile); err != nil {
				fmt.Printf("❌ 解压缩失败: %v\n", err)
				return
			}
			fmt.Println("✅ 解压缩完成")
			fmt.Println()
		}

		// 生成数据库
		fmt.Println("🔨 步骤 2/3: 生成数据库文件...")
		fmt.Println()
		if err := RunConverter(csvFile); err != nil {
			fmt.Printf("❌ 生成数据库失败: %v\n", err)
			return
		}
		fmt.Println()
		fmt.Println("✅ 步骤 3/3: 数据库初始化完成！")
		fmt.Println()
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("  🎉 初始化成功！正在启动词典...")
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println()
	} else {
		fmt.Println("✅ 数据库已就绪，正在启动...")
	}

	// 初始化数据库
	var err error
	englishDB, err = sql.Open("sqlite", englishDBFile)
	if err != nil {
		fmt.Printf("无法打开英文数据库: %v\n", err)
		return
	}
	defer englishDB.Close()

	chineseDB, err = sql.Open("sqlite", chineseDBFile)
	if err != nil {
		fmt.Printf("无法打开中文数据库: %v\n", err)
		return
	}
	defer chineseDB.Close()

	// 创建应用
	app = tview.NewApplication()

	// 用于存储搜索结果（需要在创建UI组件之前声明）
	var searchResults []string
	var searchMutex sync.Mutex // 保护搜索结果的并发访问
	var inputTimer *time.Timer // 输入后的自动切换定时器

	// 创建UI组件
	searchInput := tview.NewInputField().
		SetLabel("搜索: ").
		SetFieldWidth(0).
		SetPlaceholder("输入中文或英文...")

	wordList := tview.NewList().
		ShowSecondaryText(false).
		SetWrapAround(false). // 禁止循环滚动
		SetMainTextColor(tcell.ColorWhite).
		SetSelectedTextColor(tcell.ColorBlack).
		SetSelectedBackgroundColor(tcell.ColorYellow)

	detailView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWordWrap(true)
	detailView.SetBorder(true).SetTitle("详细信息")

	// 需求2: 监听列表选择变化，按上下键时立即显示详情
	wordList.SetChangedFunc(func(index int, mainText string, secondaryText string, shortcut rune) {
		// 只有当焦点在列表上时才响应
		if app.GetFocus() == wordList && index >= 0 {
			searchMutex.Lock()
			if index < len(searchResults) {
				selectedWord := searchResults[index]
				searchMutex.Unlock()

				// 异步加载详细信息（不添加到历史记录）
				go func(sw string) {
					var detail string
					if isChinese(sw) {
						detail = showChineseDetail(sw)
					} else {
						detail = showEnglishDetail(sw)
					}

					app.QueueUpdateDraw(func() {
						detailView.SetText(detail)
					})
				}(selectedWord)
			} else {
				searchMutex.Unlock()
			}
		}
	})

	// 显示初始单词列表的函数
	showInitialWords := func() {
		go func() {
			var results []string
			history := getSearchHistory()

			if len(history) == 0 {
				// 没有历史记录，显示随机单词
				results = getRandomWords(20)
			} else {
				// 显示搜索历史
				results = history
			}

			searchMutex.Lock()
			searchResults = results
			searchMutex.Unlock()

			app.QueueUpdateDraw(func() {
				wordList.Clear()
				for i, word := range results {
					index := i
					// 不使用颜色标记，避免影响选中颜色
					displayText := word
					if len(history) > 0 && i < len(history) {
						displayText = "★ " + word // 使用星号标记历史
					}
					wordList.AddItem(displayText, "", 0, func() {
						searchMutex.Lock()
						selectedWord := searchResults[index]
						searchMutex.Unlock()

						// 点击时不添加到历史记录，避免卡顿
						// 只在查询详情
						go func(sw string) {
							var detail string
							if isChinese(sw) {
								detail = showChineseDetail(sw)
							} else {
								detail = showEnglishDetail(sw)
							}

							app.QueueUpdateDraw(func() {
								detailView.SetText(detail)
							})
						}(selectedWord)
					})
				}
			})
		}()
	}

	// 搜索功能（异步查询）
	searchInput.SetChangedFunc(func(text string) {
		searchText := strings.TrimSpace(text)
		wordList.Clear()
		detailView.Clear()

		// 取消之前的定时器
		if inputTimer != nil {
			inputTimer.Stop()
			inputTimer = nil
		}

		if searchText == "" {
			searchMutex.Lock()
			searchResults = []string{}
			searchMutex.Unlock()
			// 显示初始单词列表（历史或随机）
			showInitialWords()
			return
		}

		// 在新的 goroutine 中异步查询
		go func(query string) {
			var results []string

			// 判断是中文还是英文
			if isChinese(query) {
				results = searchChinese(query)
			} else {
				results = searchEnglish(query)
			}

			// 更新搜索结果
			searchMutex.Lock()
			searchResults = results
			searchMutex.Unlock()

			// 在主线程中更新UI
			app.QueueUpdateDraw(func() {
				wordList.Clear()
				for i, word := range results {
					index := i // 捕获循环变量
					wordList.AddItem(word, "", 0, func() {
						searchMutex.Lock()
						selectedWord := searchResults[index]
						searchMutex.Unlock()

						// 添加到历史记录
						addToHistory(selectedWord)

						// 异步加载详细信息
						go func(sw string) {
							var detail string
							if isChinese(sw) {
								detail = showChineseDetail(sw)
							} else {
								detail = showEnglishDetail(sw)
							}

							// 在主线程中更新详细信息
							app.QueueUpdateDraw(func() {
								detailView.SetText(detail)
							})
						}(selectedWord)
					})
				}

				// 需求1: 如果有搜索结果，自动选中第一项并显示详情
				if len(results) > 0 {
					wordList.SetCurrentItem(0)
					firstWord := results[0]

					// 异步加载第一个词的详细信息
					go func(sw string) {
						var detail string
						if isChinese(sw) {
							detail = showChineseDetail(sw)
						} else {
							detail = showEnglishDetail(sw)
						}

						app.QueueUpdateDraw(func() {
							detailView.SetText(detail)
						})
					}(firstWord)
				}
			})
		}(searchText)

		// 新需求: 5秒后自动将焦点切换到单词列表，并将搜索词添加到历史
		// 重要：这个定时器在每次输入时都会被重置，只有停止输入5秒后才会触发
		inputTimer = time.AfterFunc(5*time.Second, func() {
			app.QueueUpdateDraw(func() {
				if app.GetFocus() == searchInput && searchInput.GetText() != "" {
					// 添加当前选中的词到历史记录
					searchMutex.Lock()
					if len(searchResults) > 0 {
						currentIndex := wordList.GetCurrentItem()
						if currentIndex >= 0 && currentIndex < len(searchResults) {
							addToHistory(searchResults[currentIndex])
						}
					}
					searchMutex.Unlock()

					// 切换焦点到单词列表
					app.SetFocus(wordList)
				}
			})
		})
	})

	// 设置输入框快捷键
	searchInput.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter || key == tcell.KeyDown {
			app.SetFocus(wordList)
		}
	})

	// 左侧面板（搜索框和列表）
	leftPanel := tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(searchInput, 1, 0, true).
		AddItem(wordList, 0, 1, false)
	leftPanel.SetBorder(true).SetTitle("单词列表")

	// 主布局（左右分栏）
	mainLayout := tview.NewFlex().
		AddItem(leftPanel, 0, 3, true).
		AddItem(detailView, 0, 7, false)

	// 设置全局快捷键
	mainLayout.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyEsc {
			app.Stop()
		} else if event.Key() == tcell.KeyTab {
			// Tab切换焦点
			if app.GetFocus() == searchInput {
				app.SetFocus(wordList)
			} else if app.GetFocus() == wordList {
				app.SetFocus(detailView)
			} else {
				app.SetFocus(searchInput)
			}
			return nil
		} else if event.Key() == tcell.KeyUp || event.Key() == tcell.KeyDown {
			// 新需求: 无论焦点在哪里，按上下键时切换到单词列表
			if app.GetFocus() != wordList {
				app.SetFocus(wordList)
				// 让单词列表处理这个按键事件
				return event
			}
			return event
		} else if event.Rune() != 0 && app.GetFocus() != searchInput {
			// 需求3: 当焦点不在搜索框时，敲击键盘自动切换到搜索框并清空内容
			searchInput.SetText("")
			app.SetFocus(searchInput)
			// 让搜索框处理这个字符
			return event
		}
		return event
	})

	// 初始显示随机单词或历史记录
	showInitialWords()

	// 运行应用
	if err := app.SetRoot(mainLayout, true).EnableMouse(true).Run(); err != nil {
		panic(err)
	}
}

// 添加到搜索历史
func addToHistory(word string) {
	historyMutex.Lock()
	defer historyMutex.Unlock()

	// 如果已存在，先移除
	for i, w := range searchHistory {
		if w == word {
			searchHistory = append(searchHistory[:i], searchHistory[i+1:]...)
			break
		}
	}

	// 添加到开头
	searchHistory = append([]string{word}, searchHistory...)

	// 限制历史记录大小
	if len(searchHistory) > maxHistorySize {
		searchHistory = searchHistory[:maxHistorySize]
	}
}

// 获取随机单词（bnc > 0 且 < 1000）
func getRandomWords(count int) []string {
	query := `SELECT word FROM words WHERE CAST(bnc AS INTEGER) > 0 AND CAST(bnc AS INTEGER) < 1000 ORDER BY RANDOM() LIMIT ?`

	rows, err := englishDB.Query(query, count)
	if err != nil {
		return []string{"查询出错: " + err.Error()}
	}
	defer rows.Close()

	var results []string
	for rows.Next() {
		var word string
		if err := rows.Scan(&word); err != nil {
			continue
		}
		results = append(results, word)
	}
	return results
}

// 获取搜索历史
func getSearchHistory() []string {
	historyMutex.Lock()
	defer historyMutex.Unlock()

	// 返回副本
	history := make([]string, len(searchHistory))
	copy(history, searchHistory)
	return history
}

func isChinese(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func searchEnglish(keyword string) []string {
	var results []string
	seen := make(map[string]bool) // 用于去重
	limit := 100

	// 1. 精确匹配
	query := `SELECT word FROM words WHERE word = ? LIMIT ?`
	rows, err := englishDB.Query(query, keyword, limit)
	if err == nil {
		for rows.Next() {
			var word string
			if err := rows.Scan(&word); err == nil && !seen[word] {
				results = append(results, word)
				seen[word] = true
			}
		}
		rows.Close()
	}

	// 如果已经达到限制，直接返回
	if len(results) >= limit {
		return results
	}

	// 2. 前缀匹配（排除已匹配的）
	remaining := limit - len(results)
	query = `SELECT word FROM words WHERE word LIKE ? AND word != ? LIMIT ?`
	rows, err = englishDB.Query(query, keyword+"%", keyword, remaining)
	if err == nil {
		for rows.Next() {
			var word string
			if err := rows.Scan(&word); err == nil && !seen[word] {
				results = append(results, word)
				seen[word] = true
			}
		}
		rows.Close()
	}

	// 如果已经达到限制，直接返回
	if len(results) >= limit {
		return results
	}

	// 3. 包含匹配（排除已匹配的）
	remaining = limit - len(results)
	query = `SELECT word FROM words WHERE word LIKE ? AND word NOT LIKE ? LIMIT ?`
	rows, err = englishDB.Query(query, "%"+keyword+"%", keyword+"%", remaining)
	if err == nil {
		for rows.Next() {
			var word string
			if err := rows.Scan(&word); err == nil && !seen[word] {
				results = append(results, word)
				seen[word] = true
			}
		}
		rows.Close()
	}

	return results
}

func searchChinese(keyword string) []string {
	var results []string
	seen := make(map[string]bool) // 用于去重
	limit := 100

	// 1. 精确匹配
	query := `SELECT DISTINCT chinese FROM chinese_words WHERE chinese = ? LIMIT ?`
	rows, err := chineseDB.Query(query, keyword, limit)
	if err == nil {
		for rows.Next() {
			var chinese string
			if err := rows.Scan(&chinese); err == nil && !seen[chinese] {
				results = append(results, chinese)
				seen[chinese] = true
			}
		}
		rows.Close()
	}

	// 如果已经达到限制，直接返回
	if len(results) >= limit {
		return results
	}

	// 2. 前缀匹配（排除已匹配的）
	remaining := limit - len(results)
	query = `SELECT DISTINCT chinese FROM chinese_words WHERE chinese LIKE ? AND chinese != ? LIMIT ?`
	rows, err = chineseDB.Query(query, keyword+"%", keyword, remaining)
	if err == nil {
		for rows.Next() {
			var chinese string
			if err := rows.Scan(&chinese); err == nil && !seen[chinese] {
				results = append(results, chinese)
				seen[chinese] = true
			}
		}
		rows.Close()
	}

	// 如果已经达到限制，直接返回
	if len(results) >= limit {
		return results
	}

	// 3. 包含匹配（排除已匹配的）
	remaining = limit - len(results)
	query = `SELECT DISTINCT chinese FROM chinese_words WHERE chinese LIKE ? AND chinese NOT LIKE ? LIMIT ?`
	rows, err = chineseDB.Query(query, "%"+keyword+"%", keyword+"%", remaining)
	if err == nil {
		for rows.Next() {
			var chinese string
			if err := rows.Scan(&chinese); err == nil && !seen[chinese] {
				results = append(results, chinese)
				seen[chinese] = true
			}
		}
		rows.Close()
	}

	return results
}

func showEnglishDetail(word string) string {
	query := `SELECT word, phonetic, definition, translation, bnc 
	          FROM words WHERE word = ?`

	var w Word
	err := englishDB.QueryRow(query, word).Scan(
		&w.Word, &w.Phonetic, &w.Definition, &w.Translation, &w.Bnc)

	if err != nil {
		return "查询出错: " + err.Error()
	}

	var details []string
	details = append(details, "[yellow]单词:[-] [white::b]"+w.Word+"[-]")
	details = append(details, "")

	if w.Phonetic != "" {
		details = append(details, "[yellow]音标:[-] "+w.Phonetic)
		details = append(details, "")
	}

	if w.Definition != "" {
		details = append(details, "[yellow]英文释义:[-]")
		// 将 \n 替换为实际换行，然后按行分割
		defText := strings.ReplaceAll(w.Definition, "\\n", "\n")
		defs := strings.Split(defText, "\n")
		for _, def := range defs {
			if strings.TrimSpace(def) != "" {
				details = append(details, "  [green]•[-] "+strings.TrimSpace(def))
			}
		}
		details = append(details, "")
	}

	if w.Translation != "" {
		details = append(details, "[yellow]中文释义:[-]")
		// 将 \n 替换为实际换行，然后按行分割
		transText := strings.ReplaceAll(w.Translation, "\\n", "\n")
		trans := strings.Split(transText, "\n")
		for _, tran := range trans {
			if strings.TrimSpace(tran) != "" {
				details = append(details, "  [green]•[-] "+strings.TrimSpace(tran))
			}
		}
		details = append(details, "")
	}

	if w.Bnc != "" && w.Bnc != "0" {
		details = append(details, "[yellow]BNC词频:[-] "+w.Bnc)
	}

	return strings.Join(details, "\n")
}

func showChineseDetail(chinese string) string {
	query := `SELECT english_words FROM chinese_words WHERE chinese = ?`

	var englishWords string
	err := chineseDB.QueryRow(query, chinese).Scan(&englishWords)
	if err != nil {
		return "查询出错: " + err.Error()
	}

	var details []string
	details = append(details, "[yellow]中文:[-] [white::b]"+chinese+"[-]")
	details = append(details, "")
	details = append(details, "[yellow]对应的英文单词:[-]")
	details = append(details, "")

	// 解析英文单词列表（格式：英文单词（中文释义），用换行符分隔）
	words := strings.Split(englishWords, "\n")
	for i, word := range words {
		word = strings.TrimSpace(word)
		if word == "" {
			continue
		}

		// 清除单词释义中的换行符
		word = cleanNewlines(word)

		line := fmt.Sprintf("[green]%d.[-] [cyan]%s[-]", i+1, word)
		details = append(details, line)
	}

	if len(words) == 0 {
		details = append(details, "[red]未找到对应的英文单词[-]")
	}

	return strings.Join(details, "\n")
}

// 解压缩并导入数据库文件
func importDatabase(gzFile, dbFile string) error {
	// 打开.gz文件
	f, err := os.Open(gzFile)
	if err != nil {
		return fmt.Errorf("无法打开文件 %s: %v", gzFile, err)
	}
	defer f.Close()

	// 创建一个gzip.Reader
	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("无法创建gzip.Reader: %v", err)
	}
	defer gr.Close()

	// 创建目标数据库文件
	out, err := os.Create(dbFile)
	if err != nil {
		return fmt.Errorf("无法创建文件 %s: %v", dbFile, err)
	}
	defer out.Close()

	// 将解压缩后的数据写入目标文件
	if _, err := io.Copy(out, gr); err != nil {
		return fmt.Errorf("解压缩失败: %v", err)
	}

	return nil
}
