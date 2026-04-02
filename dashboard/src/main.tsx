import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { DashboardProvider } from './state/DashboardContext';
import App from './App';
import './index.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <DashboardProvider>
      <App />
    </DashboardProvider>
  </StrictMode>,
);
