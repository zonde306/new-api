/*
Copyright (C) 2025 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/

import HeaderBar from './headerbar';
import { Layout } from '@douyinfe/semi-ui';
import SiderBar from './SiderBar';
import App from '../../App';
import FooterBar from './Footer';
import { ToastContainer } from 'react-toastify';
import React, { useContext, useEffect, useRef, useState } from 'react';
import { useIsMobile } from '../../hooks/common/useIsMobile';
import { useSidebarCollapsed } from '../../hooks/common/useSidebarCollapsed';
import { useTranslation } from 'react-i18next';
  import {
    API,
    getLogo,
    getSystemName,
    getCustomCSS,
    showError,
    showInfo,
    showSuccess,
    renderQuota,
    setStatusData,
  } from '../../helpers';

import { UserContext } from '../../context/User';
import { StatusContext } from '../../context/Status';
import { useLocation } from 'react-router-dom';
const { Sider, Content, Header } = Layout;

const PageLayout = () => {
  const [, userDispatch] = useContext(UserContext);
  const [statusState, statusDispatch] = useContext(StatusContext);
  const isMobile = useIsMobile();
  const [collapsed, , setCollapsed] = useSidebarCollapsed();
  const [drawerOpen, setDrawerOpen] = useState(false);
  const { i18n, t } = useTranslation();
  const location = useLocation();
  const autoCheckinRef = useRef(false);

  const AUTO_CHECKIN_DATE_KEY = 'auto_checkin_last_date';

  const getLocalDateKeys = (date = new Date()) => {
    const year = date.getFullYear();
    const month = String(date.getMonth() + 1).padStart(2, '0');
    const day = String(date.getDate()).padStart(2, '0');
    return {
      day: `${year}-${month}-${day}`,
      month: `${year}-${month}`,
    };
  };

  const shouldSkipAutoCheckin = (todayKey) =>
    localStorage.getItem(AUTO_CHECKIN_DATE_KEY) === todayKey;

  const markAutoCheckinDone = (todayKey) => {
    localStorage.setItem(AUTO_CHECKIN_DATE_KEY, todayKey);
  };

  const cardProPages = [
    '/console/channel',
    '/console/log',
    '/console/redemption',
    '/console/user',
    '/console/token',
    '/console/midjourney',
    '/console/task',
    '/console/models',
    '/pricing',
  ];

  const shouldHideFooter = cardProPages.includes(location.pathname);

  const shouldInnerPadding =
    location.pathname.includes('/console') &&
    !location.pathname.startsWith('/console/chat') &&
    location.pathname !== '/console/playground';

  const isConsoleRoute = location.pathname.startsWith('/console');
  const showSider = isConsoleRoute && (!isMobile || drawerOpen);

  useEffect(() => {
    if (isMobile && drawerOpen && collapsed) {
      setCollapsed(false);
    }
  }, [isMobile, drawerOpen, collapsed, setCollapsed]);

  const loadUser = () => {
    let user = localStorage.getItem('user');
    if (user) {
      let data = JSON.parse(user);
      userDispatch({ type: 'login', payload: data });
    }
  };

  const loadStatus = async () => {
    try {
      const res = await API.get('/api/status');
      const { success, data } = res.data;
      if (success) {
        statusDispatch({ type: 'set', payload: data });
        setStatusData(data);
      } else {
        showError('Unable to connect to server');
      }
    } catch (error) {
      showError('Failed to load status');
    }
  };

  useEffect(() => {
    loadUser();
    loadStatus().catch(console.error);
    let systemName = getSystemName();
    if (systemName) {
      document.title = systemName;
    }
    let logo = getLogo();
    if (logo) {
      let linkElement = document.querySelector("link[rel~='icon']");
      if (linkElement) {
        linkElement.href = logo;
      }
    }
    const savedLang = localStorage.getItem('i18nextLng');
    if (savedLang) {
      i18n.changeLanguage(savedLang);
    }
  }, [i18n]);

  useEffect(() => {
    const styleId = 'custom-css-overrides';
    const cssContent = getCustomCSS();
    let styleEl = document.getElementById(styleId);

    if (cssContent) {
      if (!styleEl) {
        styleEl = document.createElement('style');
        styleEl.id = styleId;
        styleEl.type = 'text/css';
        document.head.appendChild(styleEl);
      }
      styleEl.innerHTML = cssContent;
    } else if (styleEl) {
      styleEl.remove();
    }

    return () => {
      const el = document.getElementById(styleId);
      if (el) el.remove();
    };
  }, [statusState?.status?.custom_css]);

  useEffect(() => {
    const user = localStorage.getItem('user');
    if (!user) return;
    if (!statusState?.status?.checkin_enabled) return;

    const { day: todayKey, month } = getLocalDateKeys();
    if (shouldSkipAutoCheckin(todayKey)) return;
    if (autoCheckinRef.current) return;

    autoCheckinRef.current = true;

    const runAutoCheckin = async () => {
      try {
        const statusRes = await API.get(`/api/user/checkin?month=${month}`, {
          skipErrorHandler: true,
        });
        const { success, data, message } = statusRes.data || {};
        if (!success) {
          showError(message || t('获取签到状态失败'));
          return;
        }

        if (data?.stats?.checked_in_today) {
          // showInfo(t('今日已签到'));
          markAutoCheckinDone(todayKey);
          return;
        }

        const checkinRes = await API.post('/api/user/checkin', null, {
          skipErrorHandler: true,
        });
        const {
          success: checkinSuccess,
          data: checkinData,
          message: checkinMessage,
        } = checkinRes.data || {};
        if (checkinSuccess) {
          showSuccess(
            `${t('签到成功！获得')} ${renderQuota(checkinData?.quota_awarded || 0)}`,
          );
          markAutoCheckinDone(todayKey);
          return;
        }
        showError(checkinMessage || t('签到失败'));
        markAutoCheckinDone(todayKey);
      } catch (error) {
        showError(error);
      }
    };

    runAutoCheckin().finally(() => {
      autoCheckinRef.current = false;
    });
  }, [location.pathname, statusState?.status?.checkin_enabled, t]);

  return (
    <Layout
      className='app-layout'
      style={{
        display: 'flex',
        flexDirection: 'column',
        overflow: isMobile ? 'visible' : 'hidden',
      }}
    >
      <Header
        style={{
          padding: 0,
          height: 'auto',
          lineHeight: 'normal',
          position: 'fixed',
          width: '100%',
          top: 0,
          zIndex: 100,
        }}
      >
        <HeaderBar
          onMobileMenuToggle={() => setDrawerOpen((prev) => !prev)}
          drawerOpen={drawerOpen}
        />
      </Header>
      <Layout
        style={{
          overflow: isMobile ? 'visible' : 'auto',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        {showSider && (
          <Sider
            className='app-sider'
            style={{
              position: 'fixed',
              left: 0,
              top: '64px',
              zIndex: 99,
              border: 'none',
              paddingRight: '0',
              width: 'var(--sidebar-current-width)',
            }}
          >
            <SiderBar
              onNavigate={() => {
                if (isMobile) setDrawerOpen(false);
              }}
            />
          </Sider>
        )}
        <Layout
          style={{
            marginLeft: isMobile
              ? '0'
              : showSider
                ? 'var(--sidebar-current-width)'
                : '0',
            flex: '1 1 auto',
            display: 'flex',
            flexDirection: 'column',
          }}
        >
          <Content
            style={{
              flex: '1 0 auto',
              overflowY: isMobile ? 'visible' : 'hidden',
              WebkitOverflowScrolling: 'touch',
              padding: shouldInnerPadding ? (isMobile ? '5px' : '24px') : '0',
              position: 'relative',
            }}
          >
            <App />
          </Content>
          {!shouldHideFooter && (
            <Layout.Footer
              style={{
                flex: '0 0 auto',
                width: '100%',
              }}
            >
              <FooterBar />
            </Layout.Footer>
          )}
        </Layout>
      </Layout>
      <ToastContainer />
    </Layout>
  );
};

export default PageLayout;
