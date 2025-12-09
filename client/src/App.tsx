import { useEffect, useState } from 'react'
import './App.css'
import {
  Repo,
  WebSocketClientAdapter,
} from "@automerge/react";
import type { DocHandle } from "@automerge/automerge-repo";

// Create a single Repo instance connected to Automerge's public sync server
const repo = new Repo({
  network: [
    new WebSocketClientAdapter("wss://sync.automerge.org"),
  ],
});

interface Doc {
  text: string;
}

// Helper function to get document ID from URL
function getDocumentIdFromUrl(): string | null {
  const params = new URLSearchParams(window.location.search);
  return params.get('doc');
}

// Helper function to update URL with document ID
function updateUrlWithDocumentId(docId: string) {
  const url = new URL(window.location.href);
  url.searchParams.set('doc', docId);
  window.history.replaceState({}, '', url.toString());
}

function App() {
  const [handle, setHandle] = useState<DocHandle<Doc> | null>(null);
  const [text, setText] = useState("");

  // Initialize document handle
  useEffect(() => {
    let isMounted = true;

    async function initDoc() {
      // Check if document ID is in URL
      const urlDocId = getDocumentIdFromUrl();
      
      if (urlDocId) {
        // Use document ID from URL
        try {
          const docHandle = await repo.find<Doc>(urlDocId as any);
          await docHandle.whenReady();
          
          if (isMounted) {
            setHandle(docHandle);
            
            const doc = docHandle.docSync();
            if (doc) {
              setText(doc.text || "");
            } else {
              docHandle.change((doc: Doc) => {
                if (!doc.text) {
                  doc.text = "";
                }
              });
            }
            
            // Listen for changes from other clients
            docHandle.on("change", () => {
              if (isMounted) {
                const updatedDoc = docHandle.docSync();
                if (updatedDoc && updatedDoc.text !== undefined) {
                  setText(updatedDoc.text);
                }
              }
            });
          }
        } catch (error) {
          console.error("Error loading document from URL:", error);
          // If document can't be loaded, create a new one
          const docHandle = repo.create<Doc>();
          docHandle.change((doc: Doc) => {
            doc.text = "";
          });
          
          if (isMounted) {
            setHandle(docHandle);
            // Update URL with new document ID
            updateUrlWithDocumentId(docHandle.url);
          }
        }
      } else {
        // No document ID in URL, create a new document
        const docHandle = repo.create<Doc>();
        docHandle.change((doc: Doc) => {
          doc.text = "";
        });
        
        if (isMounted) {
          setHandle(docHandle);
          // Update URL with new document ID
          updateUrlWithDocumentId(docHandle.url);
        }
      }
    }

    initDoc();

    return () => {
      isMounted = false;
    };
  }, []);

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    const newText = e.target.value;
    setText(newText);
    
    // Update the Automerge document
    if (handle) {
      // Update URL with document ID if not already set (happens on first keystroke)
      if (!getDocumentIdFromUrl()) {
        updateUrlWithDocumentId(handle.url);
      }
      
      handle.change((doc: Doc) => {
        doc.text = newText;
      });
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
