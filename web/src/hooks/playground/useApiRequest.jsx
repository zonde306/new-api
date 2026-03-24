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

import { useCallback, useEffect, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { SSE } from 'sse.js';
import {
  API_ENDPOINTS,
  MESSAGE_STATUS,
  DEBUG_TABS,
} from '../../constants/playground.constants';
import {
  getUserIdFromLocalStorage,
  handleApiError,
  processThinkTags,
  processIncompleteThinkTags,
} from '../../helpers';

const STREAM_FLUSH_INTERVAL = 80;
const MAX_DEBUG_SSE_MESSAGES = 200;
const MAX_DEBUG_RESPONSE_CHARS = 200000;
const DEBUG_RESPONSE_TRUNCATION_PREFIX =
  '[... 为避免内存占用过高，较早的流式响应内容已被截断 ...]\n';
const DEBUG_SSE_TRUNCATION_NOTICE =
  '[... 为避免内存占用过高，较早的 SSE 消息已被省略 ...]';

export const useApiRequest = (
  setMessage,
  setDebugData,
  setActiveDebugTab,
  sseSourceRef,
  saveMessages,
) => {
  const { t } = useTranslation();
  const pendingStreamChunksRef = useRef({ content: '', reasoning: '' });
  const streamFlushTimerRef = useRef(null);

  const trimDebugResponse = useCallback((content = '') => {
    if (content.length <= MAX_DEBUG_RESPONSE_CHARS) {
      return content;
    }
    return (
      DEBUG_RESPONSE_TRUNCATION_PREFIX +
      content.slice(-MAX_DEBUG_RESPONSE_CHARS)
    );
  }, []);

  const appendDebugSSEMessage = useCallback((messages = [], data) => {
    const baseMessages = Array.isArray(messages) ? messages : [];
    const hasNotice = baseMessages[0] === DEBUG_SSE_TRUNCATION_NOTICE;
    const rawMessages = hasNotice ? baseMessages.slice(1) : baseMessages;
    const nextMessages = [...rawMessages, data];

    if (nextMessages.length <= MAX_DEBUG_SSE_MESSAGES) {
      return hasNotice
        ? [DEBUG_SSE_TRUNCATION_NOTICE, ...nextMessages]
        : nextMessages;
    }

    return [
      DEBUG_SSE_TRUNCATION_NOTICE,
      ...nextMessages.slice(-(MAX_DEBUG_SSE_MESSAGES - 1)),
    ];
  }, []);

  // 处理消息自动关闭逻辑的公共函数
  const applyAutoCollapseLogic = useCallback(
    (message, isThinkingComplete = true) => {
      const shouldAutoCollapse =
        isThinkingComplete && !message.hasAutoCollapsed;
      return {
        isThinkingComplete,
        hasAutoCollapsed: shouldAutoCollapse || message.hasAutoCollapsed,
        isReasoningExpanded: shouldAutoCollapse
          ? false
          : message.isReasoningExpanded,
      };
    },
    [],
  );

  // 流式消息更新
  const streamMessageUpdate = useCallback(
    (textChunk, type) => {
      setMessage((prevMessage) => {
        const lastMessage = prevMessage[prevMessage.length - 1];
        if (!lastMessage) return prevMessage;
        if (lastMessage.role !== 'assistant') return prevMessage;
        if (lastMessage.status === MESSAGE_STATUS.ERROR) {
          return prevMessage;
        }

        if (
          lastMessage.status === MESSAGE_STATUS.LOADING ||
          lastMessage.status === MESSAGE_STATUS.INCOMPLETE
        ) {
          let newMessage = { ...lastMessage };

          if (type === 'reasoning') {
            newMessage = {
              ...newMessage,
              reasoningContent:
                (lastMessage.reasoningContent || '') + textChunk,
              status: MESSAGE_STATUS.INCOMPLETE,
              isThinkingComplete: false,
            };
          } else if (type === 'content') {
            const newContent = (lastMessage.content || '') + textChunk;

            let shouldCollapseFromThinkTag = false;
            let thinkingCompleteFromTags = lastMessage.isThinkingComplete;

            if (
              lastMessage.isReasoningExpanded &&
              newContent.includes('</think>')
            ) {
              const thinkMatches = newContent.match(/<think>/g);
              const thinkCloseMatches = newContent.match(/<\/think>/g);
              if (
                thinkMatches &&
                thinkCloseMatches &&
                thinkCloseMatches.length >= thinkMatches.length
              ) {
                shouldCollapseFromThinkTag = true;
                thinkingCompleteFromTags = true;
              }
            }

            const isThinkingComplete =
              (lastMessage.reasoningContent &&
                !lastMessage.isThinkingComplete) ||
              thinkingCompleteFromTags;

            const autoCollapseState = applyAutoCollapseLogic(
              lastMessage,
              isThinkingComplete,
            );

            newMessage = {
              ...newMessage,
              content: newContent,
              status: MESSAGE_STATUS.INCOMPLETE,
              ...autoCollapseState,
              ...(shouldCollapseFromThinkTag && {
                isReasoningExpanded: false,
                hasAutoCollapsed: true,
              }),
            };
          }

          return [...prevMessage.slice(0, -1), newMessage];
        }

        return prevMessage;
      });
    },
    [setMessage, applyAutoCollapseLogic],
  );

  const resetPendingStreamChunks = useCallback(() => {
    pendingStreamChunksRef.current = { content: '', reasoning: '' };
    if (streamFlushTimerRef.current) {
      clearTimeout(streamFlushTimerRef.current);
      streamFlushTimerRef.current = null;
    }
  }, []);

  const flushPendingStreamChunks = useCallback(() => {
    const pendingChunks = pendingStreamChunksRef.current;
    resetPendingStreamChunks();

    if (pendingChunks.reasoning) {
      streamMessageUpdate(pendingChunks.reasoning, 'reasoning');
    }
    if (pendingChunks.content) {
      streamMessageUpdate(pendingChunks.content, 'content');
    }
  }, [resetPendingStreamChunks, streamMessageUpdate]);

  const schedulePendingStreamFlush = useCallback(() => {
    if (streamFlushTimerRef.current) {
      return;
    }

    streamFlushTimerRef.current = setTimeout(() => {
      streamFlushTimerRef.current = null;
      flushPendingStreamChunks();
    }, STREAM_FLUSH_INTERVAL);
  }, [flushPendingStreamChunks]);

  useEffect(() => {
    return () => {
      resetPendingStreamChunks();
    };
  }, [resetPendingStreamChunks]);

  // 完成消息
  const completeMessage = useCallback(
    (status = MESSAGE_STATUS.COMPLETE) => {
      setMessage((prevMessage) => {
        const lastMessage = prevMessage[prevMessage.length - 1];
        if (
          lastMessage.status === MESSAGE_STATUS.COMPLETE ||
          lastMessage.status === MESSAGE_STATUS.ERROR
        ) {
          return prevMessage;
        }

        const autoCollapseState = applyAutoCollapseLogic(lastMessage, true);

        const updatedMessages = [
          ...prevMessage.slice(0, -1),
          {
            ...lastMessage,
            status: status,
            ...autoCollapseState,
          },
        ];

        if (
          status === MESSAGE_STATUS.COMPLETE ||
          status === MESSAGE_STATUS.ERROR
        ) {
          setTimeout(() => saveMessages(updatedMessages), 0);
        }

        return updatedMessages;
      });
    },
    [setMessage, applyAutoCollapseLogic, saveMessages],
  );

  // 非流式请求
  const handleNonStreamRequest = useCallback(
    async (payload) => {
      resetPendingStreamChunks();

      setDebugData((prev) => ({
        ...prev,
        request: payload,
        timestamp: new Date().toISOString(),
        response: null,
        sseMessages: null,
        isStreaming: false,
      }));
      setActiveDebugTab(DEBUG_TABS.REQUEST);

      try {
        const response = await fetch(API_ENDPOINTS.CHAT_COMPLETIONS, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            'New-Api-User': getUserIdFromLocalStorage(),
          },
          body: JSON.stringify(payload),
        });

        if (!response.ok) {
          let errorBody = '';
          try {
            errorBody = await response.text();
          } catch (e) {
            errorBody = '无法读取错误响应体';
          }

          const errorInfo = handleApiError(
            new Error(
              `HTTP error! status: ${response.status}, body: ${errorBody}`,
            ),
            response,
          );

          setDebugData((prev) => ({
            ...prev,
            response: JSON.stringify(errorInfo, null, 2),
          }));
          setActiveDebugTab(DEBUG_TABS.RESPONSE);

          throw new Error(
            `HTTP error! status: ${response.status}, body: ${errorBody}`,
          );
        }

        const data = await response.json();

        setDebugData((prev) => ({
          ...prev,
          response: JSON.stringify(data, null, 2),
        }));
        setActiveDebugTab(DEBUG_TABS.RESPONSE);

        if (data.choices?.[0]) {
          const choice = data.choices[0];
          const content = choice.message?.content || '';
          const reasoningContent =
            choice.message?.reasoning_content ||
            choice.message?.reasoning ||
            '';

          const processed = processThinkTags(content, reasoningContent);

          setMessage((prevMessage) => {
            const newMessages = [...prevMessage];
            const lastMessage = newMessages[newMessages.length - 1];
            if (lastMessage?.status === MESSAGE_STATUS.LOADING) {
              const autoCollapseState = applyAutoCollapseLogic(
                lastMessage,
                true,
              );

              newMessages[newMessages.length - 1] = {
                ...lastMessage,
                content: processed.content,
                reasoningContent: processed.reasoningContent,
                status: MESSAGE_STATUS.COMPLETE,
                ...autoCollapseState,
              };
            }
            return newMessages;
          });
        }
      } catch (error) {
        console.error('Non-stream request error:', error);

        const errorInfo = handleApiError(error);
        setDebugData((prev) => ({
          ...prev,
          response: JSON.stringify(errorInfo, null, 2),
        }));
        setActiveDebugTab(DEBUG_TABS.RESPONSE);

        setMessage((prevMessage) => {
          const newMessages = [...prevMessage];
          const lastMessage = newMessages[newMessages.length - 1];
          if (lastMessage?.status === MESSAGE_STATUS.LOADING) {
            const autoCollapseState = applyAutoCollapseLogic(lastMessage, true);

            newMessages[newMessages.length - 1] = {
              ...lastMessage,
              content: t('请求发生错误: ') + error.message,
              status: MESSAGE_STATUS.ERROR,
              ...autoCollapseState,
            };
          }
          return newMessages;
        });
      }
    },
    [
      setDebugData,
      setActiveDebugTab,
      setMessage,
      t,
      applyAutoCollapseLogic,
      resetPendingStreamChunks,
    ],
  );

  // SSE请求
  const handleSSE = useCallback(
    (payload) => {
      resetPendingStreamChunks();

      setDebugData((prev) => ({
        ...prev,
        request: payload,
        timestamp: new Date().toISOString(),
        response: null,
        sseMessages: [],
        isStreaming: true,
      }));
      setActiveDebugTab(DEBUG_TABS.REQUEST);

      const source = new SSE(API_ENDPOINTS.CHAT_COMPLETIONS, {
        headers: {
          'Content-Type': 'application/json',
          'New-Api-User': getUserIdFromLocalStorage(),
        },
        method: 'POST',
        payload: JSON.stringify(payload),
      });

      sseSourceRef.current = source;

      let responseData = '';
      let hasReceivedFirstResponse = false;
      let isStreamComplete = false;

      source.addEventListener('message', (e) => {
        if (e.data === '[DONE]') {
          flushPendingStreamChunks();
          isStreamComplete = true;
          source.close();
          sseSourceRef.current = null;
          setDebugData((prev) => ({
            ...prev,
            response: responseData,
            sseMessages: appendDebugSSEMessage(prev.sseMessages, '[DONE]'),
            isStreaming: false,
          }));
          completeMessage();
          return;
        }

        try {
          const eventPayload = JSON.parse(e.data);
          responseData = trimDebugResponse(responseData + e.data + '\n');

          if (!hasReceivedFirstResponse) {
            setActiveDebugTab(DEBUG_TABS.RESPONSE);
            hasReceivedFirstResponse = true;
          }

          setDebugData((prev) => ({
            ...prev,
            sseMessages: appendDebugSSEMessage(prev.sseMessages, e.data),
          }));

          const delta = eventPayload.choices?.[0]?.delta;
          if (delta) {
            if (delta.reasoning_content) {
              pendingStreamChunksRef.current.reasoning += delta.reasoning_content;
            }
            if (delta.reasoning) {
              pendingStreamChunksRef.current.reasoning += delta.reasoning;
            }
            if (delta.content) {
              pendingStreamChunksRef.current.content += delta.content;
            }

            if (
              pendingStreamChunksRef.current.reasoning ||
              pendingStreamChunksRef.current.content
            ) {
              schedulePendingStreamFlush();
            }
          }
        } catch (error) {
          console.error('Failed to parse SSE message:', error);
          const errorInfo = `解析错误: ${error.message}`;

          setDebugData((prev) => ({
            ...prev,
            response: trimDebugResponse(
              responseData + `\n\nError: ${errorInfo}`,
            ),
            sseMessages: appendDebugSSEMessage(prev.sseMessages, e.data),
          }));
          setActiveDebugTab(DEBUG_TABS.RESPONSE);
        }
      });

      source.addEventListener('error', (e) => {
        if (!isStreamComplete && source.readyState !== 2) {
          flushPendingStreamChunks();
          console.error('SSE Error:', e);
          const errorMessage = e.data || t('请求发生错误');

          const errorInfo = handleApiError(new Error(errorMessage));
          errorInfo.readyState = source.readyState;

          setDebugData((prev) => ({
            ...prev,
            response: trimDebugResponse(
              responseData +
                '\n\nSSE Error:\n' +
                JSON.stringify(errorInfo, null, 2),
            ),
            isStreaming: false,
          }));
          setActiveDebugTab(DEBUG_TABS.RESPONSE);

          streamMessageUpdate(errorMessage, 'content');
          completeMessage(MESSAGE_STATUS.ERROR);
          sseSourceRef.current = null;
          source.close();
        }
      });

      source.addEventListener('readystatechange', (e) => {
        if (
          e.readyState >= 2 &&
          source.status !== undefined &&
          source.status !== 200 &&
          !isStreamComplete
        ) {
          flushPendingStreamChunks();

          const errorInfo = handleApiError(new Error('HTTP状态错误'));
          errorInfo.status = source.status;
          errorInfo.readyState = source.readyState;

          setDebugData((prev) => ({
            ...prev,
            response: trimDebugResponse(
              responseData +
                '\n\nHTTP Error:\n' +
                JSON.stringify(errorInfo, null, 2),
            ),
            isStreaming: false,
          }));
          setActiveDebugTab(DEBUG_TABS.RESPONSE);

          source.close();
          streamMessageUpdate(t('连接已断开'), 'content');
          completeMessage(MESSAGE_STATUS.ERROR);
        }
      });

      try {
        source.stream();
      } catch (error) {
        flushPendingStreamChunks();
        console.error('Failed to start SSE stream:', error);
        const errorInfo = handleApiError(error);

        setDebugData((prev) => ({
          ...prev,
          response: trimDebugResponse(
            'Stream启动失败:\n' + JSON.stringify(errorInfo, null, 2),
          ),
          isStreaming: false,
        }));
        setActiveDebugTab(DEBUG_TABS.RESPONSE);

        streamMessageUpdate(t('建立连接时发生错误'), 'content');
        completeMessage(MESSAGE_STATUS.ERROR);
      }
    },
    [
      setDebugData,
      setActiveDebugTab,
      streamMessageUpdate,
      completeMessage,
      t,
      resetPendingStreamChunks,
      flushPendingStreamChunks,
      schedulePendingStreamFlush,
      trimDebugResponse,
      appendDebugSSEMessage,
    ],
  );

  // 停止生成
  const onStopGenerator = useCallback(() => {
    flushPendingStreamChunks();

    if (sseSourceRef.current) {
      sseSourceRef.current.close();
      sseSourceRef.current = null;
    }

    setMessage((prevMessage) => {
      if (prevMessage.length === 0) return prevMessage;
      const lastMessage = prevMessage[prevMessage.length - 1];

      if (
        lastMessage.status === MESSAGE_STATUS.LOADING ||
        lastMessage.status === MESSAGE_STATUS.INCOMPLETE
      ) {
        const processed = processIncompleteThinkTags(
          lastMessage.content || '',
          lastMessage.reasoningContent || '',
        );

        const autoCollapseState = applyAutoCollapseLogic(lastMessage, true);

        const updatedMessages = [
          ...prevMessage.slice(0, -1),
          {
            ...lastMessage,
            status: MESSAGE_STATUS.COMPLETE,
            reasoningContent: processed.reasoningContent || null,
            content: processed.content,
            ...autoCollapseState,
          },
        ];

        setTimeout(() => saveMessages(updatedMessages), 0);

        return updatedMessages;
      }
      return prevMessage;
    });
  }, [
    flushPendingStreamChunks,
    setMessage,
    applyAutoCollapseLogic,
    saveMessages,
  ]);

  // 发送请求
  const sendRequest = useCallback(
    (payload, isStream) => {
      if (isStream) {
        handleSSE(payload);
      } else {
        handleNonStreamRequest(payload);
      }
    },
    [handleSSE, handleNonStreamRequest],
  );

  return {
    sendRequest,
    onStopGenerator,
    streamMessageUpdate,
    completeMessage,
  };
};

