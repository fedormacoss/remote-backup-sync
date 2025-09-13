package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"encoding/json"

	"github.com/pkg/sftp"
	"github.com/schollz/progressbar/v3"
	"golang.org/x/crypto/ssh"
)

type Config struct {
	Host         string
	User         string
	Password     string
	TargetDir    string
	BackupBase   string
	SourceDir    string
	LogFile      string
	SSHControl   string
	HostKeyAlgos []string
}

func LoadConfig(path string) (Config, error) {
	var config Config
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	return config, err
}
func main() {
	cfg, _ := LoadConfig("./config.json")

	logFile, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer logFile.Close()

	logger := log.New(logFile, "", log.LstdFlags)
	logger.Println("=========================================")
	logger.Println("Начало синхронизации")

	sshConfig := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.Password),
		},
		HostKeyCallback:   ssh.InsecureIgnoreHostKey(),
		HostKeyAlgorithms: cfg.HostKeyAlgos,
		Timeout:           30 * time.Second,
	}

	conn, err := ssh.Dial("tcp", cfg.Host+":22", sshConfig)
	if err != nil {
		logger.Fatal("Ошибка подключения SSH: ", err)
	}
	defer conn.Close()

	sftpClient, err := sftp.NewClient(conn)
	if err != nil {
		logger.Fatal("Ошибка создания SFTP клиента: ", err)
	}
	defer sftpClient.Close()

	backupDir := filepath.Join(cfg.BackupBase, time.Now().Format("20060102_150405"))
	backupCreated := false

	calculateFileHash := func(path string) (string, error) {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()

		hash := md5.New()
		if _, err := io.Copy(hash, file); err != nil {
			return "", err
		}

		return fmt.Sprintf("%x", hash.Sum(nil)), nil
	}

	calculateRemoteFileHash := func(path string) (string, error) {
		file, err := sftpClient.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()

		hash := md5.New()
		if _, err := io.Copy(hash, file); err != nil {
			return "", err
		}

		return fmt.Sprintf("%x", hash.Sum(nil)), nil
	}

	copyFile := func(srcPath, dstPath string) error {
		dstDir := filepath.Dir(dstPath)
		if err := sftpClient.MkdirAll(dstDir); err != nil {
			return err
		}

		srcFile, err := os.Open(srcPath)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		dstFile, err := sftpClient.Create(dstPath)
		if err != nil {
			return err
		}
		defer dstFile.Close()

		if _, err := dstFile.ReadFrom(srcFile); err != nil {
			return err
		}

		srcInfo, err := srcFile.Stat()
		if err != nil {
			return err
		}
		if err := sftpClient.Chmod(dstPath, srcInfo.Mode()); err != nil {
			return err
		}

		return nil
	}

	createBackup := func(remotePath, backupPath string) error {
		if err := sftpClient.MkdirAll(filepath.Dir(backupPath)); err != nil {
			return err
		}

		srcFile, err := sftpClient.Open(remotePath)
		if err != nil {
			return err
		}
		defer srcFile.Close()

		dstFile, err := sftpClient.Create(backupPath)
		if err != nil {
			return err
		}
		defer dstFile.Close()

		if _, err := dstFile.ReadFrom(srcFile); err != nil {
			return err
		}

		srcInfo, err := srcFile.Stat()
		if err != nil {
			return err
		}
		if err := sftpClient.Chmod(backupPath, srcInfo.Mode()); err != nil {
			return err
		}

		return nil
	}

	localFiles := make(map[string]os.FileInfo)
	err = filepath.Walk(cfg.SourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, err := filepath.Rel(cfg.SourceDir, path)
			if err != nil {
				return err
			}
			localFiles[relPath] = info
		}
		return nil
	})
	if err != nil {
		logger.Printf("Ошибка обхода исходной директории: %v\n", err)
		return
	}

	remoteFiles := make(map[string]os.FileInfo)
	var walkRemote func(string) error
	walkRemote = func(dir string) error {
		entries, err := sftpClient.ReadDir(dir)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			fullPath := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if err := walkRemote(fullPath); err != nil {
					return err
				}
			} else {
				relPath, err := filepath.Rel(cfg.TargetDir, fullPath)
				if err != nil {
					return err
				}
				remoteFiles[relPath] = entry
			}
		}
		return nil
	}

	if err := walkRemote(cfg.TargetDir); err != nil {
		logger.Printf("Ошибка обхода целевой директории: %v\n", err)
	}

	totalOps := len(localFiles) + (len(remoteFiles) - len(localFiles))

	bar := progressbar.NewOptions(totalOps,
		progressbar.OptionSetDescription("Синхронизация файлов"),
		progressbar.OptionSetWriter(os.Stdout),
		progressbar.OptionSetWidth(30),
		progressbar.OptionThrottle(100*time.Millisecond),
		progressbar.OptionShowCount(),
		progressbar.OptionShowElapsedTimeOnFinish(),
		progressbar.OptionOnCompletion(func() {
			fmt.Println()
		}),
		progressbar.OptionSetPredictTime(true),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "█",
			SaucerHead:    "█",
			SaucerPadding: " ",
			BarStart:      "|",
			BarEnd:        "|",
		}),
	)

	for relPath, localInfo := range localFiles {
		localPath := filepath.Join(cfg.SourceDir, relPath)
		remotePath := filepath.Join(cfg.TargetDir, relPath)
		backupPath := filepath.Join(backupDir, relPath)

		remoteInfo, err := sftpClient.Stat(remotePath)
		if err != nil {
			logger.Printf("Копируем новый файл: %s\n", relPath)
			if err := copyFile(localPath, remotePath); err != nil {
				logger.Printf("Ошибка копирования нового файла %s: %v\n", relPath, err)
			}
			bar.Add(1)
			continue
		}

		localModTime := localInfo.ModTime()
		remoteModTime := remoteInfo.ModTime()
		timeEqual := localModTime.Equal(remoteModTime) ||
			(localModTime.After(remoteModTime) && localModTime.Sub(remoteModTime) <= 2*time.Second) ||
			(localModTime.Before(remoteModTime) && remoteModTime.Sub(localModTime) <= 2*time.Second)
		sizeEqual := localInfo.Size() == remoteInfo.Size()

		if !timeEqual || !sizeEqual {
			localHash, err := calculateFileHash(localPath)
			if err != nil {
				logger.Printf("Ошибка вычисления хэша локального файла %s: %v\n", relPath, err)
				bar.Add(1)
				continue
			}

			remoteHash, err := calculateRemoteFileHash(remotePath)
			if err != nil {
				logger.Printf("Ошибка вычисления хэша удаленного файла %s: %v\n", relPath, err)
				bar.Add(1)
				continue
			}

			if localHash != remoteHash {
				logger.Printf("Файл изменен: %s (хэши различаются)\n", relPath)

				if err := createBackup(remotePath, backupPath); err != nil {
					logger.Printf("Ошибка создания бэкапа для %s: %v\n", relPath, err)
					bar.Add(1)
					continue
				}

				if err := copyFile(localPath, remotePath); err != nil {
					logger.Printf("Ошибка копирования обновленного файла %s: %v\n", relPath, err)
					bar.Add(1)
					continue
				}

				logger.Printf("Обновлен файл: %s (создан бэкап)\n", relPath)
				backupCreated = true
			} else {
				logger.Printf("Файл не изменен (хэши совпадают): %s\n", relPath)

				if err := sftpClient.Chtimes(remotePath, time.Now(), localModTime); err != nil {
					logger.Printf("Ошибка обновления времени модификации для %s: %v\n", relPath, err)
				}
			}
		} else {
			logger.Printf("Файл не изменен: %s\n", relPath)
		}
		bar.Add(1)
	}

	for relPath := range remoteFiles {
		if _, exists := localFiles[relPath]; !exists {
			remotePath := filepath.Join(cfg.TargetDir, relPath)
			backupPath := filepath.Join(backupDir, relPath)

			if err := createBackup(remotePath, backupPath); err != nil {
				logger.Printf("Ошибка создания бэкапа для удаляемого файла %s: %v\n", relPath, err)
				bar.Add(1)
				continue
			}

			if err := sftpClient.Remove(remotePath); err != nil {
				logger.Printf("Ошибка удаления файла %s: %v\n", relPath, err)
				bar.Add(1)
				continue
			}

			logger.Printf("Удален файл: %s (создан бэкап)\n", relPath)
			backupCreated = true
			bar.Add(1)
		}
	}

	if !backupCreated {
		if _, err := sftpClient.Stat(backupDir); err == nil {
			if err := sftpClient.RemoveDirectory(backupDir); err != nil {
				if strings.Contains(err.Error(), "directory not empty") {
					logger.Printf("Директория бэкапа не пуста, оставляем: %s\n", backupDir)
				} else {
					logger.Printf("Ошибка удаления директории бэкапа: %v\n", err)
				}
			} else {
				logger.Printf("Удалена пустая директория бэкапа: %s\n", backupDir)
			}
		}
	}

	bar.Finish()
	logger.Println("Завершено")
}
