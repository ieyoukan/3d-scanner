import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('#root が index.html にありません');

// StrictMode は開発時だけ働く点検モード。部品を意図的に2回描画・2回マウントして、
// 「後片付けを忘れている」「描画のたびに結果が変わる」といった不具合を早めに見つける。
// カメラの解放やオブジェクトURLの解放を正しく書けているかの確認に役立つ。
createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
