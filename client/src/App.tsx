import { useEffect, useState, useRef } from 'react'
import './App.css'
import * as Automerge from "@automerge/automerge";

interface Doc {
  text: string;
}

// Helper function to get document ID from URL
function getDocumentIdFromUrl(): string | null {
  const params = new URLSearchParams(window.location.search);
  return params.get('doc');
}

// Helper function to generate a new document ID
function generateDocumentId(): string {
  return Math.random().toString(36).substring(2, 15) + Math.random().toString(36).substring(2, 15);
}

// Helper function to update URL with document ID
function updateUrlWithDocumentId(docId: string) {
  const url = new URL(window.location.href);
  url.searchParams.set('doc', docId);
  window.history.replaceState({}, '', url.toString());
}

function App() {
  const [text, setText] = useState("");
  const docRef = useRef<Automerge.Doc<Doc> | null>(null);
  const syncStateRef = useRef<Automerge.SyncState | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const sendSyncMessageRef = useRef<(() => void) | null>(null);

  // Initialize document and websocket connection
  useEffect(() => {
    let isMounted = true;

    // Check if document ID is in URL
    let urlDocId = getDocumentIdFromUrl();
    
    if (!urlDocId) {
      // Generate a new document ID
      urlDocId = generateDocumentId();
      updateUrlWithDocumentId(urlDocId);
    }
    
    // Initialize Automerge document
    let doc = Automerge.init<Doc>();
    doc = Automerge.change(doc, (d: Doc) => {
      d.text = "";
    });
    const syncState = Automerge.initSyncState();
    
    docRef.current = doc;
    syncStateRef.current = syncState;

    // Initialize text from document
    if (doc.text) {
      setText(doc.text);
    }

    // Connect to backend websocket
    const wsUrl = `ws://localhost:8080/document/${urlDocId}`;
    const ws = new WebSocket(wsUrl);
    wsRef.current = ws;

    ws.binaryType = 'arraybuffer';

    function sendSyncMessage() {
      if (!docRef.current || !syncStateRef.current || !isMounted) {
        return;
      }

      if (ws.readyState !== WebSocket.OPEN) {
        return;
      }

      const [newSyncState, syncMessage] = Automerge.generateSyncMessage(
        docRef.current,
        syncStateRef.current
      );

      if (syncMessage) {
        syncStateRef.current = newSyncState;
        // Send as binary
        ws.send(syncMessage);
      }
    }

    function receiveSyncMessage(message: Uint8Array) {
      if (!docRef.current || !syncStateRef.current || !isMounted) {
        return;
      }

      const [newDoc, newSyncState] = Automerge.receiveSyncMessage(
        docRef.current,
        syncStateRef.current,
        message
      );

      docRef.current = newDoc;
      syncStateRef.current = newSyncState;

      // Update UI if text changed
      if (isMounted && newDoc.text !== undefined) {
        setText(newDoc.text || "");
      }

      // Send a sync message back if needed
      sendSyncMessage();
    }

    ws.onopen = () => {
      console.log('WebSocket connected');
      // Send initial sync message
      sendSyncMessage();
    };

    ws.onmessage = (event) => {
      if (event.data instanceof ArrayBuffer) {
        // Convert ArrayBuffer to Uint8Array
        const message = new Uint8Array(event.data);
        receiveSyncMessage(message);
      } else {
        console.warn('Received non-ArrayBuffer message:', event.data);
      }
    };

    ws.onerror = (error) => {
      console.error('WebSocket error:', error);
    };

    ws.onclose = (event) => {
      console.log('WebSocket disconnected', {
        code: event.code,
        reason: event.reason,
        wasClean: event.wasClean
      });
    };

    // Store sendSyncMessage function for use in handleChange
    sendSyncMessageRef.current = sendSyncMessage;

    // Periodically send sync messages (in case we have changes to sync)
    const syncInterval = setInterval(() => {
      if (ws.readyState === WebSocket.OPEN) {
        sendSyncMessage();
      }
    }, 1000);

    return () => {
      console.log('Cleaning up WebSocket connection');
      isMounted = false;
      clearInterval(syncInterval);
      if (ws && ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) {
        ws.close(1000, 'Component unmounting');
      }
      sendSyncMessageRef.current = null;
    };
  }, []);

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const newText = e.target.value;
    setText(newText);
    
    // Update the Automerge document
    if (docRef.current) {
      docRef.current = Automerge.change(docRef.current, (doc: Doc) => {
        doc.text = newText;
      });

      // Trigger sync message send
      if (sendSyncMessageRef.current) {
        sendSyncMessageRef.current();
      }
    }
  };

  return (
    <textarea
      value={text}
      onChange={handleChange}
      style={{
        width: "100vw",
        height: "100vh",
        boxSizing: "border-box",
        resize: "none",
        border: "none",
        outline: "none",
      }}
      placeholder="Start typing... (synced with Automerge)"
      autoFocus
    />
  );
}

export default App
