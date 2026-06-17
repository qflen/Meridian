import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { DashboardProvider } from './state/DashboardContext';
import App from './App';

// Self-hosted typefaces (bundled by Vite, served from /assets):
//  - Inter Tight — display grotesque for headings
//  - Inter       — neutral body sans
//  - IBM Plex Mono — tabular figures and all canvas/axis numerics
import '@fontsource/inter/400.css';
import '@fontsource/inter/500.css';
import '@fontsource/inter-tight/500.css';
import '@fontsource/inter-tight/600.css';
import '@fontsource/inter-tight/700.css';
import '@fontsource/ibm-plex-mono/400.css';
import '@fontsource/ibm-plex-mono/500.css';

import './index.css';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <DashboardProvider>
      <App />
    </DashboardProvider>
  </StrictMode>,
);
