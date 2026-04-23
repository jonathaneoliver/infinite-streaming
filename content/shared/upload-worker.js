/**
 * Shared Worker for Background Upload
 * Handles chunked file uploads across page navigation with queuing support
 */

// Upload queue management
const uploadQueue = []; // Array of {jobId, file, config, startOffset}
let activeUploadJobId = null; // Currently uploading job
const activeUploads = new Map(); // jobId -> {file, bytesUploaded, totalBytes, status}
const connectedPorts = new Set();

// Constants
const CHUNK_SIZE = 5 * 1024 * 1024; // 5MB chunks
const MAX_QUEUE_SIZE = 5;

/**
 * Handle new connection from page
 */
self.onconnect = (e) => {
    const port = e.ports[0];
    connectedPorts.add(port);
    
    console.log('[UploadWorker] New connection established');
    
    port.onmessage = async (event) => {
        const { type, jobId, file, fileSize, fileName, config, startOffset } = event.data;
        
        console.log(`[UploadWorker] Received message: ${type}`, event.data);
        
        try {
            switch(type) {
                case 'START_UPLOAD':
                    await handleStartUpload(jobId, file, fileSize, fileName, config, startOffset, port);
                    break;
                    
                case 'GET_STATUS':
                    sendStatus(jobId, port);
                    break;
                    
                case 'GET_QUEUE_STATUS':
                    sendQueueStatus(port);
                    break;
                    
                case 'CANCEL_UPLOAD':
                    cancelUpload(jobId, port);
                    break;
                    
                default:
                    console.warn(`[UploadWorker] Unknown message type: ${type}`);
            }
        } catch (error) {
            console.error(`[UploadWorker] Error handling ${type}:`, error);
            port.postMessage({
                type: 'ERROR',
                jobId,
                error: error.message
            });
        }
    };
    
    // Clean up on disconnect
    port.addEventListener('close', () => {
        connectedPorts.delete(port);
        console.log('[UploadWorker] Port disconnected');
    });
};

/**
 * Handle START_UPLOAD message
 */
async function handleStartUpload(jobId, file, fileSize, fileName, config, startOffset, port) {
    console.log(`[UploadWorker] handleStartUpload - jobId: ${jobId}, fileSize: ${fileSize}, fileName: ${fileName}`);
    
    // Check if already uploading
    if (activeUploadJobId !== null && activeUploadJobId !== jobId) {
        // Check queue limit
        if (uploadQueue.length >= MAX_QUEUE_SIZE) {
            port.postMessage({
                type: 'ERROR',
                jobId,
                error: `Queue is full (max ${MAX_QUEUE_SIZE}). Please wait for current uploads to complete.`
            });
            return;
        }
        
        // Add to queue
        uploadQueue.push({ jobId, file, fileSize, fileName, config, startOffset });
        
        console.log(`[UploadWorker] Upload queued: ${jobId} (position ${uploadQueue.length})`);
        
        // Notify queued
        broadcastToAll({
            type: 'QUEUED',
            jobId,
            position: uploadQueue.length,
            filename: file.name
        });
        
        return;
    }
    
    // Check if this job is already active (reconnection)
    if (activeUploadJobId === jobId) {
        console.log(`[UploadWorker] Job ${jobId} already active, sending current status`);
        sendStatus(jobId, port);
        return;
    }
    
    // Start upload immediately
    activeUploadJobId = jobId;
    await performUpload(jobId, file, fileSize, fileName, config, startOffset);
}

/**
 * Perform the actual chunked upload
 */
async function performUpload(jobId, file, fileSize, fileName, config, startOffset) {
    console.log(`[UploadWorker] Starting upload for job ${jobId}, size: ${fileSize}, offset: ${startOffset}`);
    console.log(`[UploadWorker] File object type: ${typeof file}, has slice: ${typeof file.slice === 'function'}`);
    
    // Check if file object is valid
    if (!file || typeof file.slice !== 'function') {
        console.error(`[UploadWorker] Invalid file object received! Type: ${typeof file}`);
        broadcastToAll({
            type: 'ERROR',
            jobId,
            error: 'Invalid file object - File cannot be transferred to SharedWorker'
        });
        activeUploadJobId = null;
        return;
    }
    
    // Use explicit fileSize instead of file.size (which may be corrupted after postMessage)
    const totalBytes = fileSize || file.size;
    console.log(`[UploadWorker] Total bytes to upload: ${totalBytes}`);
    
    // Store upload state
    activeUploads.set(jobId, {
        file,
        totalBytes: totalBytes,
        bytesUploaded: startOffset || 0,
        status: 'uploading'
    });
    
    try {
        // Upload in chunks
        let chunkCount = 0;
        for (let start = startOffset || 0; start < totalBytes; start += CHUNK_SIZE) {
            chunkCount++;
            
            // Check if cancelled
            const uploadState = activeUploads.get(jobId);
            if (!uploadState || uploadState.status === 'cancelled') {
                console.log(`[UploadWorker] Upload cancelled: ${jobId}`);
                return;
            }
            
            const end = Math.min(start + CHUNK_SIZE, totalBytes);
            
            // Re-validate File object before each chunk (especially important around chunk 16)
            if (!file || typeof file.slice !== 'function') {
                console.error(`[UploadWorker] CRITICAL: File object became invalid at chunk #${chunkCount}!`);
                throw new Error(`File object became invalid at chunk ${chunkCount}`);
            }
            
            console.log(`[UploadWorker] Slicing chunk #${chunkCount}: ${start}-${end} for job ${jobId}, file.size=${file.size}`);
            const chunk = file.slice(start, end);
            console.log(`[UploadWorker] Chunk created, size: ${chunk.size} bytes`);
            
            const formData = new FormData();
            formData.append('file', chunk);
            
            console.log(`[UploadWorker] Uploading chunk #${chunkCount} (${start}-${end}) for job ${jobId}`);
            
            // Extra logging around chunk 16-17 transition
            if (chunkCount >= 15 && chunkCount <= 18) {
                console.warn(`[UploadWorker] *** CRITICAL CHUNK #${chunkCount} - Monitoring closely ***`);
            }
            
            try {
                const response = await fetch(`/api/upload/chunk/${jobId}`, {
                    method: 'POST',
                    body: formData
                });
                
                console.log(`[UploadWorker] Chunk #${chunkCount} fetch completed, status: ${response.status}`);
                
                if (!response.ok) {
                    const errorText = await response.text();
                    throw new Error(`Chunk upload failed: ${response.status} ${errorText}`);
                }
                
                const data = await response.json();
                console.log(`[UploadWorker] Chunk #${chunkCount} uploaded successfully, progress: ${data.progress}%`);
                
                // Update state
                const upload = activeUploads.get(jobId);
                if (upload) {
                    upload.bytesUploaded = data.received_size;
                    upload.lastActivity = Date.now(); // Keep track of activity
                }
                
                // Broadcast progress to ALL connected ports (this also serves as keep-alive)
                broadcastToAll({
                    type: 'PROGRESS',
                    jobId,
                    bytesUploaded: data.received_size,
                    totalBytes: totalBytes,
                    progress: data.progress
                });
            } catch (chunkError) {
                console.error(`[UploadWorker] ERROR uploading chunk #${chunkCount}:`, chunkError);
                throw chunkError; // Re-throw to be caught by outer try/catch
            }
        }
        
        console.log(`[UploadWorker] All chunks uploaded! Total chunks: ${chunkCount}`);
        console.log(`[UploadWorker] Completing upload for job ${jobId}`);
        
        const completeResponse = await fetch(`/api/upload/complete/${jobId}`, {
            method: 'POST'
        });
        
        if (!completeResponse.ok) {
            throw new Error(`Failed to complete upload: ${completeResponse.status}`);
        }
        
        // Clean up
        activeUploads.delete(jobId);
        activeUploadJobId = null;
        
        // Notify completion
        broadcastToAll({
            type: 'COMPLETE',
            jobId
        });
        
        console.log(`[UploadWorker] Upload complete: ${jobId}`);
        
        // Process next in queue
        await processNextInQueue();
        
    } catch (error) {
        console.error(`[UploadWorker] Upload error for job ${jobId}:`, error);
        
        activeUploads.delete(jobId);
        activeUploadJobId = null;
        
        broadcastToAll({
            type: 'ERROR',
            jobId,
            error: error.message
        });
        
        // Process next in queue even after error
        await processNextInQueue();
    }
}

/**
 * Process next upload in queue
 */
async function processNextInQueue() {
    if (uploadQueue.length > 0) {
        const next = uploadQueue.shift();
        
        console.log(`[UploadWorker] Processing next in queue: ${next.jobId}`);
        
        // Notify queue updated
        broadcastToAll({
            type: 'QUEUE_UPDATED',
            queueLength: uploadQueue.length
        });
        
        activeUploadJobId = next.jobId;
        await performUpload(next.jobId, next.file, next.fileSize, next.fileName, next.config, next.startOffset);
    } else {
        console.log('[UploadWorker] Queue is empty');
    }
}

/**
 * Send status for specific job
 */
function sendStatus(jobId, port) {
    const upload = activeUploads.get(jobId);
    
    if (!upload) {
        // Check if in queue
        const queuePos = uploadQueue.findIndex(u => u.jobId === jobId);
        if (queuePos >= 0) {
            port.postMessage({
                type: 'QUEUED',
                jobId,
                position: queuePos + 1,
                filename: uploadQueue[queuePos].file.name
            });
        } else {
            port.postMessage({
                type: 'NOT_FOUND',
                jobId
            });
        }
        return;
    }
    
    const progress = upload.totalBytes > 0 
        ? Math.min(Math.round((upload.bytesUploaded / upload.totalBytes) * 100), 99)
        : 0;
    
    port.postMessage({
        type: 'PROGRESS',
        jobId,
        bytesUploaded: upload.bytesUploaded,
        totalBytes: upload.totalBytes,
        progress
    });
}

/**
 * Send queue status
 */
function sendQueueStatus(port) {
    port.postMessage({
        type: 'QUEUE_STATUS',
        activeJobId: activeUploadJobId,
        queueLength: uploadQueue.length,
        queue: uploadQueue.map(u => ({
            jobId: u.jobId,
            filename: u.file.name,
            size: u.file.size
        }))
    });
}

/**
 * Cancel upload
 */
function cancelUpload(jobId, port) {
    // Check if active
    if (activeUploadJobId === jobId) {
        const upload = activeUploads.get(jobId);
        if (upload) {
            upload.status = 'cancelled';
        }
        activeUploads.delete(jobId);
        activeUploadJobId = null;
        
        broadcastToAll({
            type: 'CANCELLED',
            jobId
        });
        
        console.log(`[UploadWorker] Upload cancelled: ${jobId}`);
        
        // Process next in queue
        processNextInQueue();
        return;
    }
    
    // Check if in queue
    const queueIndex = uploadQueue.findIndex(u => u.jobId === jobId);
    if (queueIndex >= 0) {
        uploadQueue.splice(queueIndex, 1);
        
        broadcastToAll({
            type: 'CANCELLED',
            jobId
        });
        
        broadcastToAll({
            type: 'QUEUE_UPDATED',
            queueLength: uploadQueue.length
        });
        
        console.log(`[UploadWorker] Queued upload cancelled: ${jobId}`);
        return;
    }
    
    port.postMessage({
        type: 'NOT_FOUND',
        jobId
    });
}

/**
 * Broadcast message to all connected ports
 */
function broadcastToAll(message) {
    const disconnectedPorts = [];
    
    connectedPorts.forEach(port => {
        try {
            port.postMessage(message);
        } catch (e) {
            console.warn('[UploadWorker] Failed to send to port, marking for removal:', e);
            disconnectedPorts.push(port);
        }
    });
    
    // Clean up disconnected ports
    disconnectedPorts.forEach(port => {
        connectedPorts.delete(port);
    });
}

console.log('[UploadWorker] Shared Worker initialized');
