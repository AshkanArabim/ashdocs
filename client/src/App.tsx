import { useState } from 'react'
import reactLogo from './assets/react.svg'
import viteLogo from '/vite.svg'
import './App.css'

function App() {
  const [value, setValue] = useState("");
  return (
    <textarea
      value={value}
      onChange={(e) => setValue(e.target.value)}
      style={{
        width: "100vw",
        height: "100vh",
        boxSizing: "border-box",
        resize: "none",
        border: "none",
        outline: "none",
      }}
      placeholder="Start typing..."
      autoFocus
    />
  );
}

export default App
