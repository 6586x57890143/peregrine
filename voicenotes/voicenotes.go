package voicenotes

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// TranscribeVoiceNote downloads a Discord voice-note (.ogg), converts it to .wav, and transcribes it.
func TranscribeVoiceNote(url string) (string, error) {
	// --- Prepare audio directory ---
	audioDir := filepath.Join("voicenotes", "audio")
	absAudioDir, err := filepath.Abs(audioDir)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute audio path: %w", err)
	}
	if err := os.MkdirAll(absAudioDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create audio folder: %w", err)
	}

	// --- Sanitize filename ---
	baseName := filepath.Base(url)
	if qIdx := strings.Index(baseName, "?"); qIdx != -1 {
		baseName = baseName[:qIdx]
	}
	if !strings.HasSuffix(strings.ToLower(baseName), ".ogg") {
		baseName += ".ogg"
	}
	re := regexp.MustCompile(`[<>:"/\\|?*]`)
	baseName = re.ReplaceAllString(baseName, "_")

	inPath := filepath.Join(absAudioDir, baseName)

	// --- Download file ---
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	outFile, err := os.Create(inPath)
	if err != nil {
		return "", fmt.Errorf("file create failed: %w", err)
	}
	if _, err := io.Copy(outFile, resp.Body); err != nil {
		_ = outFile.Close()
		return "", fmt.Errorf("file save failed: %w", err)
	}
	if err := outFile.Close(); err != nil {
		return "", fmt.Errorf("file close failed: %w", err)
	}

	// --- Convert .ogg → .wav ---
	wavPath := strings.TrimSuffix(inPath, filepath.Ext(inPath)) + ".wav"
	ffmpegBin, err := getAbsBinPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg path error: %w", err)
	}

	cmdConv := exec.Command(ffmpegBin, "-y", "-i", inPath, "-ar", "16000", "-ac", "1", wavPath)
	if convOut, err := cmdConv.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg convert failed: %v: %s", err, string(convOut))
	}

	// --- Prepare Whisper binary and model ---
	whisperBin, err := getAbsBinPath("whisper-cli")
	if err != nil {
		return "", fmt.Errorf("whisper-cli path error: %w", err)
	}
	modelPath := filepath.Join("voicenotes", "models", "ggml-small.bin")
	modelPath, err = filepath.Abs(modelPath)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute model path: %w", err)
	}

	// --- Transcribe with Whisper ---
	txtPath := wavPath + ".txt" // whisper outputs voice-message.wav.txt
	cmdWh := exec.Command(whisperBin, "-m", modelPath, "-f", wavPath, "-otxt")
	cmdWh.Dir = absAudioDir // ensure output goes into audio folder
	if wOut, err := cmdWh.CombinedOutput(); err != nil {
		return "", fmt.Errorf("whisper failed: %v: %s", err, string(wOut))
	}

	// --- Read transcript ---
	data, err := os.ReadFile(txtPath)
	if err != nil {
		return "", fmt.Errorf("read transcript failed: %w", err)
	}

	// --- Cleanup ---
	_ = os.Remove(inPath)
	_ = os.Remove(wavPath)
	_ = os.Remove(txtPath)

	return strings.TrimSpace(string(data)), nil
}

// getAbsBinPath returns the absolute path to a binary for the current OS.
func getAbsBinPath(name string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	binPath := filepath.Join(wd, "voicenotes", "bin", runtime.GOOS, name+ext)
	absPath, err := filepath.Abs(binPath)
	if err != nil {
		return "", err
	}

	// Check if the binary actually exists
	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return "", fmt.Errorf("binary not found: %s", absPath)
	}

	return absPath, nil
}
