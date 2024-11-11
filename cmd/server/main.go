package main

import (
    "bytes"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "os/exec"
    "sync"
    "syscall"
)

const (
    podYamlPath = "/tmp/pod.yaml"
    envFilePath = "/tmp/env"
)

// TPM measurement simulation - in real implementation, replace with actual TPM calls
func measureIntoPCR(filepath string, pcrIndex int) error {
    // Note: This is a placeholder. Replace with actual TPM measurement code
    log.Printf("Measuring %s into PCR[%d]", filepath, pcrIndex)
    return nil
}

// Atomic file write using rename
func atomicWriteFile(filename string, data []byte) error {
    tempFile := filename + ".tmp"
    
    // Create temp file
    f, err := os.OpenFile(tempFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
    if err != nil {
        return fmt.Errorf("failed to create temp file: %v", err)
    }
    defer f.Close()
    
    // Write data
    if _, err := f.Write(data); err != nil {
        os.Remove(tempFile)
        return fmt.Errorf("failed to write temp file: %v", err)
    }
    
    // Sync to ensure data is written to disk
    if err := f.Sync(); err != nil {
        os.Remove(tempFile)
        return fmt.Errorf("failed to sync temp file: %v", err)
    }
    
    // Atomic rename
    if err := os.Rename(tempFile, filename); err != nil {
        os.Remove(tempFile)
        return fmt.Errorf("failed to rename temp file: %v", err)
    }
    
    return nil
}

// Check if a file exists
func fileExists(filename string) bool {
    _, err := os.Stat(filename)
    return err == nil
}

func main() {
    var wg sync.WaitGroup
    shutdownCh := make(chan struct{})
    
    // File upload handler
    http.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }
        
        // Parse multipart form
        err := r.ParseMultipartForm(10 << 20) // 10 MB limit
        if err != nil {
            http.Error(w, "Failed to parse form", http.StatusBadRequest)
            return
        }
        
        // Handle pod.yaml
        podFile, _, err := r.FormFile("pod.yaml")
        if err != nil {
            http.Error(w, "pod.yaml is required", http.StatusBadRequest)
            return
        }
        defer podFile.Close()
        
        // Check if pod.yaml already exists
        if fileExists(podYamlPath) {
            http.Error(w, "pod.yaml already exists", http.StatusConflict)
            return
        }
        
        // Read pod.yaml content
        podContent, err := io.ReadAll(podFile)
        if err != nil {
            http.Error(w, "Failed to read pod.yaml", http.StatusInternalServerError)
            return
        }
        
        // Handle optional env file
        var envContent []byte
        if envFile, _, err := r.FormFile("env"); err == nil {
            defer envFile.Close()
            
            // Check if env already exists
            if fileExists(envFilePath) {
                http.Error(w, "env already exists", http.StatusConflict)
                return
            }
            
            envContent, err = io.ReadAll(envFile)
            if err != nil {
                http.Error(w, "Failed to read env", http.StatusInternalServerError)
                return
            }
        }
        
        // Atomic write of pod.yaml
        if err := atomicWriteFile(podYamlPath, podContent); err != nil {
            http.Error(w, fmt.Sprintf("Failed to write pod.yaml: %v", err), http.StatusInternalServerError)
            return
        }
        
        // Measure pod.yaml into PCR[13]
        if err := measureIntoPCR(podYamlPath, 13); err != nil {
            http.Error(w, "Failed to measure pod.yaml", http.StatusInternalServerError)
            return
        }
        
        // If env was provided, write it atomically and measure it
        if len(envContent) > 0 {
            if err := atomicWriteFile(envFilePath, envContent); err != nil {
                http.Error(w, fmt.Sprintf("Failed to write env: %v", err), http.StatusInternalServerError)
                return
            }
            
            // Measure env into PCR[14]
            if err := measureIntoPCR(envFilePath, 14); err != nil {
                http.Error(w, "Failed to measure env", http.StatusInternalServerError)
                return
            }
        }
        
        w.WriteHeader(http.StatusCreated)
    })
    
    // Start container handler
    http.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
            return
        }
        
        // Check if required files exist
        if !fileExists(podYamlPath) {
            http.Error(w, "pod.yaml not found", http.StatusNotFound)
            return
        }
        
        // Prepare command
        var cmd *exec.Cmd
        if fileExists(envFilePath) {
            // Start with environment file
            cmd = exec.Command("sh", "-c", fmt.Sprintf(". %s && podman play kube %s", envFilePath, podYamlPath))
        } else {
            // Start without environment file
            cmd = exec.Command("podman", "play", "kube", podYamlPath)
        }

        // Check if podman is installed
        if _, err := exec.LookPath("podman"); err != nil {
            http.Error(w, "podman is not installed", http.StatusInternalServerError)
            return
        }

        // Create buffers for output
        var stdout, stderr bytes.Buffer
        cmd.Stdout = &stdout
        cmd.Stderr = &stderr
        
        // Set process group ID to ensure child processes survive
        cmd.SysProcAttr = &syscall.SysProcAttr{
            Setpgid: true,
        }
        
	// Execute command and wait for completion
        err := cmd.Run()  // Run() combines Start() and Wait()
        if err != nil {
            errorMsg := fmt.Sprintf("Container start failed:\nStdout: %s\nStderr: %s\nError: %v",
                stdout.String(),
                stderr.String(),
                err)
            log.Printf("Error starting container: %s", errorMsg)
            http.Error(w, errorMsg, http.StatusInternalServerError)
	    // we could shutdown the server here, but I don't see any benefits
            return
        }

        log.Printf("Container started successfully. Output: %s", stdout.String())
        
        // Trigger server shutdown
        close(shutdownCh)
        w.WriteHeader(http.StatusOK)
    })
    
    // Start server
    server := &http.Server{
        Addr: ":24070",
    }
    
    // Handle graceful shutdown
    wg.Add(1)
    go func() {
        defer wg.Done()
        <-shutdownCh
        log.Println("Shutting down server...")
        server.Close()
    }()
    
    // Start the server
    log.Println("Server starting on :8080")
    if err := server.ListenAndServe(); err != http.ErrServerClosed {
        log.Fatalf("Server error: %v", err)
    }
    
    // Wait for shutdown to complete
    wg.Wait()
    log.Println("Server shutdown complete")
}
