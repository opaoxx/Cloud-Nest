import { createRoot } from 'react-dom/client'
import { App as AntApp, ConfigProvider } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import 'antd/dist/reset.css'
import App from './App'
import AuthPage from './AuthPage'
import './styles.css'

createRoot(document.getElementById('root')!).render(
  <ConfigProvider
    locale={zhCN}
    theme={{ token: {
      colorPrimary: '#27765e', borderRadius: 10, colorText: '#25352e',
      colorBorder: '#e4eae6',
      fontFamily: 'Inter, "Segoe UI", "PingFang SC", "Microsoft YaHei UI", sans-serif',
    } }}
  >
    <AntApp>{import.meta.env.VITE_CLOUDNEST_DATA_MODE === 'api' ? <AuthPage /> : <App />}</AntApp>
  </ConfigProvider>,
)
