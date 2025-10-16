package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	_ "modernc.org/sqlite"
)

// parseBNC 解析BNC词频，将空值、无效值或0视为最大值（排在最后）
func parseBNC(bnc string) int {
	bnc = strings.TrimSpace(bnc)
	if bnc == "" {
		return 1 << 30 // 返回一个很大的数
	}
	n, err := strconv.Atoi(bnc)
	if err != nil || n == 0 {
		return 1 << 30 // 无效或为0，排在最后
	}
	return n
}

// CreateEnglishDB 创建英文到中文的数据库（优化并发版本）
func CreateEnglishDB(csvFile, dbFile string) error {
	fmt.Println("   📖 [1/2] 正在创建英文-中文数据库...")

	// 打开CSV文件
	file, err := os.Open(csvFile)
	if err != nil {
		return fmt.Errorf("无法打开CSV文件: %v", err)
	}
	defer file.Close()

	// 创建SQLite数据库，启用 WAL 模式以支持并发
	db, err := sql.Open("sqlite", dbFile+"?cache=shared&mode=rwc&_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("无法创建数据库: %v", err)
	}
	defer db.Close()

	// 设置连接池参数
	db.SetMaxOpenConns(1) // SQLite 写入最好用单连接
	db.SetMaxIdleConns(1)

	// 创建表 - 只保留必要字段
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS words (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		word TEXT NOT NULL,
		phonetic TEXT,
		definition TEXT,
		translation TEXT,
		bnc TEXT
	);
	`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		return fmt.Errorf("无法创建表: %v", err)
	}

	// 创建索引 - 移除 frq 索引
	indexSQL := `
	CREATE INDEX IF NOT EXISTS idx_word ON words(word);
	CREATE INDEX IF NOT EXISTS idx_bnc ON words(CAST(bnc AS INTEGER));
	`
	_, err = db.Exec(indexSQL)
	if err != nil {
		return fmt.Errorf("无法创建索引: %v", err)
	}

	// 优化 SQLite 性能
	_, err = db.Exec("PRAGMA synchronous = NORMAL")
	if err != nil {
		return fmt.Errorf("无法设置PRAGMA: %v", err)
	}

	// 读取CSV数据
	reader := csv.NewReader(file)

	// 跳过表头
	_, err = reader.Read()
	if err != nil {
		return fmt.Errorf("无法读取表头: %v", err)
	}

	// 读取所有记录到内存
	fmt.Print("      ⏳ 正在读取词典数据...")
	var allRecords [][]string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(record) >= 13 {
			allRecords = append(allRecords, record)
		}
	}
	fmt.Printf(" 完成 (%d 条)\n", len(allRecords))

	// 并发处理参数
	numWorkers := 4   // 减少工作协程数，因为数据库写入是瓶颈
	batchSize := 1000 // 增加批次大小以提高效率

	// 用于统计
	var totalCount int64
	totalRecords := int64(len(allRecords))
	var wg sync.WaitGroup
	var dbMutex sync.Mutex // 添加互斥锁保护数据库写入

	// 创建通道用于分发任务
	recordChan := make(chan [][]string, numWorkers)

	// 进度显示
	fmt.Print("      📊 处理进度: [")
	progressDots := 50
	lastProgress := 0
	var progressMutex sync.Mutex // 保护进度条的并发更新

	// 启动工作协程
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for batch := range recordChan {
				// 使用互斥锁保护数据库操作
				dbMutex.Lock()

				// 开始事务
				tx, err := db.Begin()
				if err != nil {
					dbMutex.Unlock()
					continue
				}

				insertSQL := `INSERT INTO words (word, phonetic, definition, translation, bnc) 
							  VALUES (?, ?, ?, ?, ?)`
				stmt, err := tx.Prepare(insertSQL)
				if err != nil {
					tx.Rollback()
					dbMutex.Unlock()
					continue
				}

				batchCount := 0
				for _, record := range batch {
					_, err = stmt.Exec(
						record[0], // word
						record[1], // phonetic
						record[2], // definition
						record[3], // translation
						record[8], // bnc
					)
					if err != nil {
						continue
					}
					batchCount++
				}

				stmt.Close()

				// 提交事务
				err = tx.Commit()
				dbMutex.Unlock()

				// 更新计数和进度条
				newCount := atomic.AddInt64(&totalCount, int64(batchCount))
				progress := int(float64(newCount) / float64(totalRecords) * float64(progressDots))
				percentage := int(float64(newCount) / float64(totalRecords) * 100)

				// 更新进度条（需要加锁，因为多个协程可能同时更新）
				progressMutex.Lock()
				if progress > lastProgress {
					// 清除之前的显示并重新绘制
					fmt.Print("\r      📊 处理进度: [")
					for i := 0; i < progress; i++ {
						fmt.Print("█")
					}
					for i := progress; i < progressDots; i++ {
						fmt.Print(" ")
					}
					fmt.Printf("] %d%%", percentage)
					lastProgress = progress
				}
				progressMutex.Unlock()
			}
		}(i)
	}

	// 分批发送数据
	for i := 0; i < len(allRecords); i += batchSize {
		end := i + batchSize
		if end > len(allRecords) {
			end = len(allRecords)
		}
		recordChan <- allRecords[i:end]
	}

	close(recordChan)
	wg.Wait()

	// 补全进度条
	for i := lastProgress; i < progressDots; i++ {
		fmt.Print("█")
	}
	fmt.Printf("] 100%%\n")
	fmt.Printf("      ✅ 英文数据库创建完成 (共 %d 条记录)\n", totalCount)
	return nil
}

// extractChineseWords 从翻译中提取纯中文词汇
func extractChineseWords(translation string) []string {
	// 去除括号内容
	bracketRegex := regexp.MustCompile(`\[([^\]]*)\]|\(([^)]*)\)`)
	cleanTranslation := bracketRegex.ReplaceAllString(translation, "")

	// 分割符号
	separators := regexp.MustCompile(`[,，、;；\s]+`)
	parts := separators.Split(cleanTranslation, -1)

	var chineseWords []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// 只保留纯中文的部分
		if isAllChinese(part) {
			chineseWords = append(chineseWords, part)
		}
	}

	return chineseWords
}

// isAllChinese 检查字符串是否全部是中文字符
func isAllChinese(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		// 只允许汉字和常见中文标点
		if !unicode.Is(unicode.Han, r) && r != '·' && r != '—' {
			return false
		}
	}
	return true
}

// CreateChineseDB 创建中文到英文的反向数据库（优化版本，每个中文词一行记录）
func CreateChineseDB(csvFile, dbFile string) error {
	fmt.Println("   📖 [2/2] 正在创建中文-英文数据库...")

	// 打开CSV文件
	file, err := os.Open(csvFile)
	if err != nil {
		return fmt.Errorf("无法打开CSV文件: %v", err)
	}
	defer file.Close()

	// 创建SQLite数据库，启用 WAL 模式
	db, err := sql.Open("sqlite", dbFile+"?cache=shared&mode=rwc&_journal_mode=WAL")
	if err != nil {
		return fmt.Errorf("无法创建数据库: %v", err)
	}
	defer db.Close()

	// 设置连接池参数
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// 创建表 - 每个中文词一行，所有英文单词存在一个字段中
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS chinese_words (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chinese TEXT NOT NULL UNIQUE,
		english_words TEXT NOT NULL
	);
	`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		return fmt.Errorf("无法创建表: %v", err)
	}

	// 创建索引
	indexSQL := `
	CREATE INDEX IF NOT EXISTS idx_chinese ON chinese_words(chinese);
	`
	_, err = db.Exec(indexSQL)
	if err != nil {
		return fmt.Errorf("无法创建索引: %v", err)
	}

	// 优化 SQLite 性能
	_, err = db.Exec("PRAGMA synchronous = NORMAL")
	if err != nil {
		return fmt.Errorf("无法设置PRAGMA: %v", err)
	}

	// 读取CSV数据
	reader := csv.NewReader(file)

	// 跳过表头
	_, err = reader.Read()
	if err != nil {
		return fmt.Errorf("无法读取表头: %v", err)
	}

	// 读取所有记录到内存
	fmt.Print("      ⏳ 正在读取词典数据...")
	var allRecords [][]string
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue
		}
		if len(record) >= 13 && record[3] != "" {
			allRecords = append(allRecords, record)
		}
	}
	fmt.Printf(" 完成 (%d 条)\n", len(allRecords))

	fmt.Print("      🔄 正在构建反向索引...")
	// 构建中文词到英文单词的映射
	// key: 中文词, value: map[英文单词]中文释义
	chineseMap := make(map[string]map[string]string)

	for _, record := range allRecords {
		englishWord := record[0]
		translation := record[3]

		// 提取纯中文词汇
		chineseWords := extractChineseWords(translation)

		// 为每个中文词建立反向映射
		for _, chWord := range chineseWords {
			if chineseMap[chWord] == nil {
				chineseMap[chWord] = make(map[string]string)
			}
			// 如果该英文单词还没有记录，或者当前翻译更完整，则更新
			if _, exists := chineseMap[chWord][englishWord]; !exists {
				chineseMap[chWord][englishWord] = translation
			}
		}
	}
	fmt.Printf(" 完成 (%d 个中文词)\n", len(chineseMap))

	// 创建英文单词到BNC词频的映射，提高查询效率
	bncMap := make(map[string]int)
	for _, record := range allRecords {
		englishWord := record[0]
		if _, exists := bncMap[englishWord]; !exists {
			bncMap[englishWord] = parseBNC(record[8])
		}
	}

	// 开始事务
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("无法开始事务: %v", err)
	}

	insertSQL := `INSERT INTO chinese_words (chinese, english_words) VALUES (?, ?)`
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("无法准备语句: %v", err)
	}
	defer stmt.Close()

	// 进度显示
	fmt.Print("      📊 写入进度: [")
	progressDots := 50
	lastProgress := 0
	totalWords := len(chineseMap)
	count := 0

	for chWord, engMap := range chineseMap {
		// 收集英文单词信息：单词、释义、BNC词频
		type engInfo struct {
			word string
			def  string
			bnc  int
		}
		var engList []engInfo

		for engWord, cnTranslation := range engMap {
			// 从BNC映射中获取词频，默认为最大值
			bnc, exists := bncMap[engWord]
			if !exists {
				bnc = 1 << 30
			}
			engList = append(engList, engInfo{
				word: engWord,
				def:  cnTranslation,
				bnc:  bnc,
			})
		}

		// 按BNC词频升序排序（词频越小越靠前，0或无效排在最后）
		sort.Slice(engList, func(i, j int) bool {
			return engList[i].bnc < engList[j].bnc
		})

		// 构建英文单词列表，格式：英文单词（中文释义）
		var englishEntries []string
		for _, info := range engList {
			entry := fmt.Sprintf("%s（%s）", info.word, info.def)
			englishEntries = append(englishEntries, entry)
		}

		// 用换行符连接所有英文单词
		englishWords := strings.Join(englishEntries, "\n")

		_, err = stmt.Exec(chWord, englishWords)
		if err != nil {
			continue
		}

		count++

		// 更新进度条和百分比
		progress := int(float64(count) / float64(totalWords) * float64(progressDots))
		percentage := int(float64(count) / float64(totalWords) * 100)

		if progress > lastProgress || count%100 == 0 {
			// 清除当前行并重新绘制进度条
			fmt.Print("\r      📊 写入进度: [")
			for i := 0; i < progress; i++ {
				fmt.Print("█")
			}
			for i := progress; i < progressDots; i++ {
				fmt.Print(" ")
			}
			fmt.Printf("] %d%%", percentage)
			lastProgress = progress
		}
	}

	// 确保显示100%
	fmt.Print("\r      📊 写入进度: [")
	for i := 0; i < progressDots; i++ {
		fmt.Print("█")
	}
	fmt.Printf("] 100%%\n")

	// 提交事务
	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("提交事务失败: %v", err)
	}

	fmt.Printf("      ✅ 中文数据库创建完成 (共 %d 个中文词)\n", count)
	return nil
}

// RunConverter 执行转换操作
func RunConverter(csvFile string) error {
	fmt.Println("开始创建英文到中文数据库...")
	err := CreateEnglishDB(csvFile, "english_chinese.db")
	if err != nil {
		return fmt.Errorf("创建英文数据库失败: %v", err)
	}

	fmt.Println("\n开始创建中文到英文反向数据库...")
	err = CreateChineseDB(csvFile, "chinese_english.db")
	if err != nil {
		return fmt.Errorf("创建中文反向数据库失败: %v", err)
	}

	fmt.Println("\n所有数据库创建完成！")
	fmt.Println("- english_chinese.db: 英文到中文翻译")
	fmt.Println("- chinese_english.db: 中文到英文翻译")
	return nil
}
